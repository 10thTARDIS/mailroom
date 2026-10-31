// SPDX-License-Identifier: AGPL-3.0-only

// A RECIPIENT CHIP'S CONTACT (#816): the chip's name is a small disclosure that
// says whether this person is in your contacts and offers the one thing to do
// about it — "Open contact" for a saved one, "Add to contacts" for anyone else
// the server knows. It is the chip's own text, so it costs no extra tab stop
// beyond the name a keyboard user already crosses, and nothing is asked of the
// server until it is opened (a message with forty recipients does not make
// forty lookups to draw its chips).
//
// A disclosure (aria-expanded) rather than a menu: what it opens is sometimes
// one sentence and no action ("no mail with this address yet"), which a
// role="menu" cannot hold. Escape closes it and puts focus back on the name —
// and stops there, so it does not also close the composer.

import { useEffect, useId, useRef, useState } from "react";
import type { JSX } from "react";

import type { Contact } from "../../../core/jmap/types";
import { formatRoute, withCompose } from "../../../core/routes";
import type { Route } from "../../../core/routes";
import { navigate, useRoute } from "../../routing";
import { useApp } from "../../state";
import { t } from "../../i18n";

type Lookup = { kind: "asking" } | { kind: "cannot" } | { kind: "none" } | { kind: "found"; contact: Contact };

export function ChipContact({ address, label }: { address: string; label: string }): JSX.Element {
  const { client, actions } = useApp();
  const route = useRoute();
  const [open, setOpen] = useState(false);
  const [lookup, setLookup] = useState<Lookup>({ kind: "asking" });
  const [busy, setBusy] = useState(false);
  const panelId = useId();
  const descId = useId();
  const wrapRef = useRef<HTMLSpanElement | null>(null);
  const buttonRef = useRef<HTMLButtonElement | null>(null);

  useEffect(() => {
    if (!open) return;
    let cancelled = false;
    setLookup({ kind: "asking" });
    void client.contactForAddress(address).then((c) => {
      if (cancelled) return;
      if (c === undefined || (c !== null && typeof c.isSaved !== "boolean")) setLookup({ kind: "cannot" });
      else setLookup(c === null ? { kind: "none" } : { kind: "found", contact: c });
    });
    return () => {
      cancelled = true;
    };
  }, [open, address, client]);

  useEffect(() => {
    if (!open) return;
    const onDown = (ev: PointerEvent): void => {
      if (wrapRef.current?.contains(ev.target as Node)) return;
      setOpen(false);
    };
    document.addEventListener("pointerdown", onDown);
    return () => document.removeEventListener("pointerdown", onDown);
  }, [open]);

  const close = (): void => {
    setOpen(false);
    buttonRef.current?.focus();
  };

  const target: Route | null =
    lookup.kind === "found" ? withCompose({ page: "contacts", id: lookup.contact.id }, route.compose ?? null) : null;

  const add = async (c: Contact): Promise<void> => {
    setBusy(true);
    const ok = await actions.setContactMarkers(c.id, { isSaved: true }, t("contacts.popover.failed"));
    setBusy(false);
    if (ok) setLookup({ kind: "found", contact: { ...c, isSaved: true } });
  };

  return (
    <span
      className="compose-chip-contact"
      ref={wrapRef}
      onKeyDown={(ev) => {
        if (ev.key === "Escape" && open) {
          ev.preventDefault();
          ev.stopPropagation();
          close();
        }
      }}
    >
      <button
        type="button"
        ref={buttonRef}
        className="compose-chip-name"
        aria-expanded={open}
        aria-controls={panelId}
        aria-describedby={descId}
        title={address}
        onClick={(ev) => {
          // The chip row focuses the text field on any click; this one is ours.
          ev.stopPropagation();
          setOpen((v) => !v);
        }}
      >
        {label}
      </button>
      <span id={descId} hidden>
        {t("contacts.chipMenu.label", { name: label })}
      </span>
      <span id={panelId} className="compose-chip-panel" hidden={!open} onClick={(ev) => ev.stopPropagation()}>
        {!open ? null : lookup.kind === "asking" ? (
          <span role="status">{t("contacts.chipMenu.asking")}</span>
        ) : lookup.kind === "cannot" ? (
          <span>{t("contacts.chipMenu.cannot")}</span>
        ) : lookup.kind === "none" ? (
          <span>{t("contacts.chipMenu.none")}</span>
        ) : lookup.contact.isSaved && target ? (
          <a
            href={formatRoute(target)}
            onClick={(ev) => {
              if (ev.button !== 0 || ev.metaKey || ev.ctrlKey || ev.shiftKey || ev.altKey) return;
              ev.preventDefault();
              setOpen(false);
              navigate(target);
            }}
          >
            {t("contacts.chipMenu.open")}
          </a>
        ) : (
          <button
            type="button"
            className="button button-quiet"
            disabled={busy}
            onClick={() => void add(lookup.contact)}
          >
            {busy ? t("contacts.popover.adding") : t("contacts.chipMenu.add")}
          </button>
        )}
      </span>
    </span>
  );
}
