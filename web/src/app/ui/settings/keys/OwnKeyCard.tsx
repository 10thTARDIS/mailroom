// SPDX-License-Identifier: AGPL-3.0-only

// ONE OF YOUR KEYS (#796): what it is, what it is for, when it stops, and the
// verbs that apply to it in the state it is in.
//
//   live      choose its address · publish / withdraw · extend · replace ·
//             back up · retire · revoke
//   retired   withdraw (if still published) · back up · revoke
//   revoked   withdraw (WKD serves the revocation while it is published) ·
//             back up
//
// Extend, replace, back up and revoke need the private half, so they need the
// crypto process; on a server without one (`keyManagement: false`) they are
// not drawn and the page says why once. Choosing an address, publishing and
// retiring are store writes and work either way.
//
// THE TITLE IS THE KEY'S PRIMARY USER ID (#812), "Name <address>", with the
// others beneath and revoked ones marked: three keys with one address are
// otherwise told apart only by fingerprint. A user ID is text from wherever the
// key was made: text nodes only, never markup, and a long one is cut with the
// whole value in a tooltip. The address the key is USED for has its own line,
// and when the key's user IDs do not name that address the card says so,
// neutrally (#812): correspondents' mail apps may refuse or flag it.
//
// A retired key says when it was retired, or that the date was not recorded (a
// key retired before v1.93) — never an invented date (#815). A replaced key
// links to the key that replaced it (#817).

import { useId, useState } from "react";
import type { JSX } from "react";

import { isPastKey, retiredFact, titleUserIds, userIdText } from "../../../../core/keyLifecycle";
import {
  boundAddress,
  expiryFact,
  fingerprintTail,
  fixedIdentities,
  isRetired,
  keyChips,
  keyDateLong,
  publishBlock,
} from "../../../../core/userKeys";
import type { KeyActionError, KeyOutcome } from "../../../../core/userKeys";
import { groupFingerprint } from "../../../../core/pgpSignature";
import type { Id, Identity, UserKey } from "../../../../core/jmap/types";
import { t } from "../../../i18n";
import type { KeysApi } from "./api";
import { BackupKeyDialog } from "./BackupKeyDialog";
import { ExtendKeyDialog } from "./ExtendKeyDialog";
import { KeyConfirmDialog } from "./KeyDialog";
import { ReplaceKeyDialog } from "./ReplaceKeyDialog";
import { RevokeKeyDialog } from "./RevokeKeyDialog";
import { PUBLISH_BLOCK_TEXT, keyChipText, keyErrorText } from "./text";

type Open = null | "extend" | "replace" | "backup" | "retire" | "revoke";

/** The id a key card's heading carries, for "Replaced by" to move to. */
export function keyHeadingId(id: Id): string {
  return `key-${id}`;
}

/** How another of your keys is named in a "Replaced by" link: its primary
 * user ID when it has one, and always the fingerprint's tail. */
function keyLinkText(key: UserKey | undefined): string {
  if (!key) return t("keys.history.replacedByKey", { tail: "…" });
  const tail = t("keys.history.replacedByKey", { tail: fingerprintTail(key.fingerprint) });
  const { title } = titleUserIds(key.userIds);
  return title ? `${userIdText(title)}, ${tail}` : tail;
}

export function OwnKeyCard({
  keyItem,
  identities,
  wkdConfigured,
  wkdDomain,
  keyManagement,
  warnDays,
  now,
  api,
  onChanged,
  findKey = () => undefined,
  onShowKey = () => {},
}: {
  keyItem: UserKey;
  identities: readonly Identity[];
  wkdConfigured: boolean | null;
  wkdDomain: string | null;
  keyManagement: boolean;
  warnDays: number | null;
  now: Date;
  api: KeysApi;
  /** A change landed: reload, and say this. */
  onChanged: (message: string) => void;
  /** Another of your keys, by id (for "Replaced by"). */
  findKey?: (id: Id) => UserKey | undefined;
  /** Move to another key's card. */
  onShowKey?: (id: Id) => void;
}): JSX.Element {
  const k = keyItem;
  const [open, setOpen] = useState<Open>(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<KeyActionError | null>(null);
  const bound = boundAddress(k, identities);
  const [bindTo, setBindTo] = useState<Id | "">(k.identityId ?? "");
  const bindId = useId();
  const pubWhyId = useId();
  const retired = isRetired(k);
  const fixed = fixedIdentities(identities);
  const fact = expiryFact(k);
  const chips = keyChips(k, identities, warnDays, now);
  const block = publishBlock(k, wkdConfigured);
  const label = bound ?? groupFingerprint(k.fingerprint).slice(-9);
  const { title, others } = titleUserIds(k.userIds);
  const titleText = title ? userIdText(title) : null;
  const retiredWhen = retiredFact(k);

  const run = async <T,>(op: () => Promise<KeyOutcome<T>>, done: string | ((v: T) => string)): Promise<boolean> => {
    setBusy(true);
    setError(null);
    const out = await op();
    setBusy(false);
    if (!out.ok) {
      setError(out.error);
      return false;
    }
    api.afterChange();
    onChanged(typeof done === "function" ? done(out.value) : done);
    return true;
  };

  const expiryLine =
    fact.kind === "never"
      ? t("keys.neverExpires")
      : fact.kind === "expires"
        ? t("keys.expires", { date: keyDateLong(fact.at) })
        : fact.kind === "expired"
          ? fact.at
            ? t("keys.expiredOn", { date: keyDateLong(fact.at) })
            : t("keys.expiredNoDate")
          : null;

  return (
    <li className={isPastKey(k) ? "own-key is-past" : "own-key"}>
      <h5 className="own-key-heading" id={keyHeadingId(k.id)} tabIndex={-1}>
        {titleText ? (
          <span className="key-uid" title={titleText}>
            {titleText}
          </span>
        ) : bound ? (
          t("keys.keyFor", { address: bound })
        ) : (
          t("keys.keyUnbound")
        )}
      </h5>
      {others.length > 0 ? (
        <ul className="key-uids" aria-label={t("keys.userIds.others")}>
          {others.map((u, i) => {
            const text = userIdText(u);
            return (
              <li key={`${i}:${text}`} className={u.revoked ? "key-uid is-revoked" : "key-uid"} title={text}>
                {text}
                {u.revoked ? ` ${t("keys.userIds.revoked")}` : null}
              </li>
            );
          })}
        </ul>
      ) : null}
      {/* A replaced key keeps its identity on the server (ADR-040 §14) but is
          used for nothing: a retired key says only that it is retired. */}
      {titleText && !retired ? (
        <p className="field-hint own-key-used">{bound ? t("keys.usedFor", { address: bound }) : t("keys.usedForNone")}</p>
      ) : null}
      {bound && k.boundAddressListed === false && !retired ? (
        <p className="field-hint key-not-listed">{t("keys.notListed", { address: bound })}</p>
      ) : null}
      <p className="own-key-fingerprint">
        <span className="field-label">{t("keys.fingerprint")}</span>{" "}
        <code className="setting-fingerprint">{groupFingerprint(k.fingerprint)}</code>
      </p>
      <p className="own-key-facts">
        <span>{t("keys.created", { date: keyDateLong(k.createdAt) })}</span>
        {expiryLine ? (
          <>
            <span className="setting-sep"> · </span>
            <span className={fact.kind === "expired" ? "is-warn" : undefined}>{expiryLine}</span>
          </>
        ) : null}
        {retiredWhen.kind === "on" ? (
          <>
            <span className="setting-sep"> · </span>
            <span>{t("keys.history.retiredOn", { date: keyDateLong(retiredWhen.at) })}</span>
          </>
        ) : retiredWhen.kind === "notRecorded" ? (
          <>
            <span className="setting-sep"> · </span>
            <span>{t("keys.history.retiredNotRecorded")}</span>
          </>
        ) : null}
        {k.revokedAt ? (
          <>
            <span className="setting-sep"> · </span>
            <span>{t("keys.revokedOn", { date: keyDateLong(k.revokedAt) })}</span>
          </>
        ) : null}
      </p>
      {k.replacedBy ? (
        <p className="own-key-replaced">
          {t("keys.history.replacedBy")}{" "}
          <button type="button" className="link-button" onClick={() => onShowKey(k.replacedBy as Id)}>
            {keyLinkText(findKey(k.replacedBy))}
          </button>
        </p>
      ) : null}
      {chips.length > 0 ? (
        <ul className="key-chips" aria-label={t("keys.chips")}>
          {chips.map((c) => (
            <li
              key={c.kind + (c.kind === "signsAs" ? c.address : "")}
              className={
                c.kind === "expired" || c.kind === "expiringSoon" || c.kind === "revoked"
                  ? "remote-allow-kind is-warn"
                  : "remote-allow-kind"
              }
            >
              {keyChipText(c)}
            </li>
          ))}
        </ul>
      ) : null}
      {k.origin === "generated" && keyManagement ? (
        <p className="field-hint">{t("keys.backupHint")}</p>
      ) : null}

      {!retired ? (
        <div className="own-key-bind">
          <label htmlFor={bindId} className="field-label">
            {t("keys.bind.label")}
          </label>
          <select
            id={bindId}
            className="select"
            value={bindTo}
            disabled={busy}
            onChange={(ev) => setBindTo(ev.target.value)}
          >
            <option value="">{t("keys.bind.none")}</option>
            {fixed.map((i) => (
              <option key={i.id} value={i.id}>
                {i.email}
              </option>
            ))}
          </select>
          <button
            type="button"
            className="button"
            disabled={busy || bindTo === (k.identityId ?? "")}
            onClick={() =>
              void run(
                () => api.update(k.id, { identityId: bindTo === "" ? null : bindTo }),
                (updated) => {
                  if (bindTo === "") return t("keys.done.unbound");
                  const address = fixed.find((i) => i.id === bindTo)?.email ?? "";
                  // #812: the bind is made either way. A key that does not
                  // name the address says so in the same sentence.
                  return updated.warning === "addressNotInKey" || updated.boundAddressListed === false
                    ? t("keys.done.boundNotListed", { address })
                    : t("keys.done.bound", { address });
                },
              )
            }
          >
            {t("keys.bind.save")}
          </button>
        </div>
      ) : null}

      <div className="own-key-publish">
        {k.wkdPublished ? (
          <>
            {k.wkdUrl ? (
              <p className="field-hint">
                {t("keys.publish.at")} <code className="key-url">{k.wkdUrl}</code>
              </p>
            ) : null}
            <button
              type="button"
              className="button"
              disabled={busy}
              onClick={() => void run(() => api.update(k.id, { wkdPublished: false }), t("keys.done.unpublished"))}
            >
              {t("keys.publish.withdraw")}
            </button>
          </>
        ) : !retired ? (
          <>
            <button
              type="button"
              className="button"
              disabled={busy || block !== null}
              aria-describedby={block !== null ? pubWhyId : undefined}
              onClick={() => void run(() => api.update(k.id, { wkdPublished: true }), t("keys.done.published"))}
            >
              {t("keys.publish.publish")}
            </button>
            {block !== null ? (
              <p className="field-hint" id={pubWhyId}>
                {t(PUBLISH_BLOCK_TEXT[block], { domain: wkdDomain ?? "" })}
              </p>
            ) : k.wkdUrl ? (
              <p className="field-hint">
                {t("keys.publish.willBeAt")} <code className="key-url">{k.wkdUrl}</code>
              </p>
            ) : null}
          </>
        ) : null}
      </div>

      <div className="own-key-actions" role="group" aria-label={t("keys.actionsFor", { key: label })}>
        {keyManagement && k.revokedAt === null && !k.decryptOnly ? (
          <button type="button" className="button" disabled={busy} onClick={() => setOpen("extend")}>
            {t("keys.action.extend")}
          </button>
        ) : null}
        {keyManagement && k.revokedAt === null && !k.decryptOnly && bound !== null ? (
          <button type="button" className="button" disabled={busy} onClick={() => setOpen("replace")}>
            {t("keys.action.replace")}
          </button>
        ) : null}
        {keyManagement ? (
          <button type="button" className="button" disabled={busy} onClick={() => setOpen("backup")}>
            {t("keys.action.backup")}
          </button>
        ) : null}
        {!retired ? (
          <button type="button" className="button" disabled={busy} onClick={() => setOpen("retire")}>
            {t("keys.action.retire")}
          </button>
        ) : null}
        {keyManagement && k.revokedAt === null ? (
          <button
            type="button"
            className="button button-danger"
            disabled={busy}
            onClick={() => setOpen("revoke")}
          >
            {t("keys.action.revoke")}
          </button>
        ) : null}
      </div>

      {error && open === null ? (
        <p className="field-error" role="alert">
          {keyErrorText(error)}
        </p>
      ) : null}

      {open === "extend" ? (
        <ExtendKeyDialog
          keyItem={k}
          address={bound}
          now={now}
          warnDays={warnDays}
          onClose={() => setOpen(null)}
          onExtend={async (to) => {
            const out = await api.extend(k.id, to);
            if (!out.ok) return out;
            api.afterChange();
            onChanged(
              out.value.expiresAt
                ? t("keys.done.extended", { date: keyDateLong(out.value.expiresAt) })
                : t("keys.done.extendedNoDate"),
            );
            return { ok: true };
          }}
        />
      ) : null}
      {open === "replace" && bound !== null ? (
        <ReplaceKeyDialog
          keyItem={k}
          address={bound}
          now={now}
          warnDays={warnDays}
          onClose={() => setOpen(null)}
          onReplace={async (expiry) => {
            const out = await api.replace(k.id, expiry);
            if (!out.ok) {
              // A new key was made but not put in place: list it now. The
              // dialog stays open with the same sentence.
              if (out.leftNewKey !== null) {
                api.afterChange();
                onChanged(t("keys.replace.leftNewKey"));
              }
              return { ok: false, error: out.error, leftNewKey: out.leftNewKey !== null };
            }
            api.afterChange();
            onChanged(
              out.value.published
                ? t("keys.done.replacedPublished", { address: bound })
                : t("keys.done.replaced", { address: bound }),
            );
            return { ok: true };
          }}
        />
      ) : null}
      {open === "backup" ? (
        <BackupKeyDialog
          address={bound}
          onClose={() => setOpen(null)}
          onExport={(passphrase) => api.exportKey(k.id, passphrase)}
          onDone={(fileName) => onChanged(t("keys.done.backedUp", { file: fileName }))}
        />
      ) : null}
      {open === "retire" ? (
        <KeyConfirmDialog
          title={t("keys.retire.title")}
          paragraphs={[
            t("keys.retire.what"),
            ...(bound ? [t("keys.retire.unbinds", { address: bound })] : []),
            t("keys.retire.permanent"),
          ]}
          confirmLabel={t("keys.retire.submit")}
          busyLabel={t("keys.retire.busy")}
          busy={busy}
          error={error ? keyErrorText(error) : null}
          onClose={() => {
            setError(null);
            setOpen(null);
          }}
          onConfirm={() =>
            void run(
              () =>
                api.update(k.id, k.identityId !== null ? { decryptOnly: true, identityId: null } : { decryptOnly: true }),
              t("keys.done.retired"),
            ).then((ok) => {
              if (ok) setOpen(null);
            })
          }
        />
      ) : null}
      {open === "revoke" ? (
        <RevokeKeyDialog
          keyItem={k}
          address={bound}
          onClose={() => setOpen(null)}
          onRevoke={async (reason, confirm) => {
            const out = await api.revoke(k.id, reason, confirm);
            if (!out.ok) return out;
            api.afterChange();
            onChanged(t("keys.done.revoked"));
            return { ok: true };
          }}
        />
      ) : null}
    </li>
  );
}
