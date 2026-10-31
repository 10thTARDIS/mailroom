// SPDX-License-Identifier: AGPL-3.0-only

// RECIPIENT CHIPS (#448): To / Cc / Bcc as chips that carry the key fact.
//
// KEEPS THE AUTOCOMPLETE IT REPLACED. The dropdown over `Contact/query`, its
// keyboard handling and its mousedown-before-blur trick are the ones
// `RecipientInput` had (#37); what changed is that picking or finishing an address
// produces a CHIP rather than text, which is what makes `⚿ key held` sayable at
// all — you cannot annotate a substring.
//
// THE KEY FACT IS NOT COMPUTED HERE. `chipKeyFact` (./chips.ts) decides it from a
// Contact, and the three-way answer — held / none / unknown — is the whole point:
// `unknown` prints NOTHING, because "no key" for a correspondent nobody asked
// about is a claim nobody made (#176).
//
// THE LOOKUP IS THE PARENT'S JOB. Compose owns the per-address cache, because the
// SEND LINE needs the same facts and two caches would be two answers. This
// component asks for a lookup (`onNeedFact`) and renders whatever the parent
// knows.

import { useEffect, useId, useRef, useState } from "react";
import type { JSX } from "react";

import type { Contact, ContactAddress } from "../../../core/jmap/types";
import { useApp } from "../../state";
import { t } from "../../i18n";
import { addChips, chipLabel, looksLikeAddress, parseChips } from "./chips";
import type { Chip, ChipKeyFact } from "./chips";
import { ChipContact } from "./ChipContact";

export function RecipientChips({
  label,
  chips,
  onChips,
  factFor,
  onNeedFact,
  onPick,
  keysAvailable,
  autoFocus = false,
}: {
  label: string;
  chips: readonly Chip[];
  onChips: (next: Chip[]) => void;
  /** What the parent knows about an address, or "unknown". */
  factFor: (address: string) => ChipKeyFact;
  /** "I have a new address; resolve its key fact." The Contact is passed when the
   * chip came from a pick, so the parent can cache it without a round trip. */
  onNeedFact: (address: string, known?: Contact) => void;
  /** Reported so the From hint (spec §5's alias model, run backwards) still works:
   * a contact who once reached the user at an alias defaults the From to it. */
  onPick?: (addr: ContactAddress) => void;
  /** Whether this server can answer the key question at all. False means no chip
   * prints a key fact — not that every recipient lacks one. */
  keysAvailable: boolean;
  autoFocus?: boolean;
}): JSX.Element {
  const { client } = useApp();
  const [text, setText] = useState("");
  const [suggestions, setSuggestions] = useState<Contact[]>([]);
  const [open, setOpen] = useState(false);
  const [active, setActive] = useState(0);
  const debounce = useRef<ReturnType<typeof setTimeout> | undefined>(undefined);
  const blurTimer = useRef<ReturnType<typeof setTimeout> | undefined>(undefined);
  const inputRef = useRef<HTMLInputElement>(null);
  const listId = useId();

  useEffect(() => {
    if (autoFocus) inputRef.current?.focus();
  }, [autoFocus]);

  // Debounced autocomplete on what is being typed. A short token or any error
  // yields no dropdown — typing is never blocked on the network.
  useEffect(() => {
    const token = text.trim();
    if (token.length < 2) {
      setSuggestions([]);
      setOpen(false);
      return;
    }
    if (debounce.current) clearTimeout(debounce.current);
    debounce.current = setTimeout(() => {
      void client
        .contactAutocomplete(token)
        .then((cs) => {
          setSuggestions(cs);
          setActive(0);
          setOpen(cs.length > 0);
        })
        .catch(() => {
          setSuggestions([]);
          setOpen(false);
        });
    }, 160);
    return () => {
      if (debounce.current) clearTimeout(debounce.current);
    };
  }, [text, client]);

  useEffect(
    () => () => {
      if (blurTimer.current) clearTimeout(blurTimer.current);
    },
    [],
  );

  /** Adds chips and asks the parent to resolve each new address. */
  const commit = (added: readonly Chip[], known?: Contact): void => {
    if (added.length === 0) return;
    const next = addChips(chips, added);
    onChips(next);
    for (const chip of added) onNeedFact(chip.address, known);
  };

  const chooseContact = (c: Contact): void => {
    const picked = c.addresses[0];
    if (!picked?.address) return;
    // The Contact is handed along: the pick already IS the answer to the key
    // question, so resolving it again would be a round trip for data in hand.
    commit([{ address: picked.address, name: c.displayName || null }], c);
    setText("");
    setOpen(false);
    setSuggestions([]);
    onPick?.(picked);
  };

  /** Everything the input has that is finished, chipped; the tail left behind. */
  const flush = (value: string): void => {
    const { chips: parsed, rest } = parseChips(value);
    commit(parsed);
    setText(rest);
  };

  const onKeyDown = (ev: React.KeyboardEvent<HTMLInputElement>): void => {
    // Ctrl/⌘+Enter is SEND from anywhere in the composer, suggestion list open or
    // not: it bubbles to the form rather than quietly picking a contact.
    if (ev.key === "Enter" && (ev.ctrlKey || ev.metaKey)) return;

    if (open && suggestions.length > 0) {
      if (ev.key === "ArrowDown") {
        ev.preventDefault();
        setActive((i) => (i + 1) % suggestions.length);
        return;
      }
      if (ev.key === "ArrowUp") {
        ev.preventDefault();
        setActive((i) => (i - 1 + suggestions.length) % suggestions.length);
        return;
      }
      if (ev.key === "Enter") {
        ev.preventDefault();
        const c = suggestions[active];
        if (c) chooseContact(c);
        return;
      }
      if (ev.key === "Escape") {
        // The list only. The composer's own Escape handler is capture-phase and
        // would otherwise close the whole composer over a dropdown.
        ev.preventDefault();
        ev.stopPropagation();
        setOpen(false);
        return;
      }
    }

    if (ev.key === "Enter" || ev.key === "," || ev.key === ";") {
      const value = text.trim();
      if (value === "") {
        // Enter on an empty field submits the form (that is what Enter does in a
        // form); a stray comma does nothing.
        if (ev.key !== "Enter") ev.preventDefault();
        return;
      }
      ev.preventDefault();
      flush(`${value},`);
      return;
    }

    // BACKSPACE ON AN EMPTY FIELD EDITS THE LAST CHIP rather than deleting it
    // outright: a chip is text the user typed, and one keystroke that silently
    // removes an address is how a recipient goes missing from a message. It comes
    // back as text, ready to fix.
    if (ev.key === "Backspace" && text === "" && chips.length > 0) {
      ev.preventDefault();
      const last = chips[chips.length - 1] as Chip;
      onChips(chips.slice(0, -1));
      setText(last.name ? `${last.name} <${last.address}>` : last.address);
    }
  };

  return (
    <div className="field compose-chips-field">
      <span className="field-label">{label}</span>
      <div className="compose-chips" onClick={() => inputRef.current?.focus()}>
        {chips.map((chip) => {
          const fact = keysAvailable ? factFor(chip.address) : "unknown";
          return (
            <span
              key={chip.address.toLowerCase()}
              className={`compose-chip compose-chip-${fact}`}
              title={chip.address}
            >
              {/* #816: the name opens what the chip knows about the contact. */}
              <ChipContact address={chip.address} label={chipLabel(chip)} />
              {/* NOTHING AT ALL for `unknown`. The chip is silent rather than
                  reassuring or alarming about a question that has no answer. */}
              {fact === "held" ? (
                <span className="compose-chip-key">
                  <span aria-hidden="true">⚿ </span>
                  {t("compose.chip.keyHeld")}
                </span>
              ) : null}
              {fact === "expired" ? (
                <span className="compose-chip-lapsed">{t("compose.chip.keyExpired")}</span>
              ) : null}
              {fact === "revoked" ? (
                <span className="compose-chip-lapsed">{t("compose.chip.keyRevoked")}</span>
              ) : null}
              {fact === "none" ? (
                <span className="compose-chip-nokey">{t("compose.chip.noKey")}</span>
              ) : null}
              <button
                type="button"
                className="compose-chip-remove"
                aria-label={t("compose.chip.remove", { name: chipLabel(chip) })}
                onClick={(ev) => {
                  ev.stopPropagation();
                  onChips(chips.filter((c) => c !== chip));
                }}
              >
                ✕
              </button>
            </span>
          );
        })}
        <span className="compose-chip-input">
          <input
            ref={inputRef}
            className="compose-chip-text"
            value={text}
            role="combobox"
            aria-label={label}
            aria-expanded={open}
            aria-controls={listId}
            aria-autocomplete="list"
            aria-activedescendant={open && suggestions[active] ? `${listId}-${active}` : undefined}
            autoComplete="off"
            onChange={(ev) => {
              const value = ev.target.value;
              // A PASTE OF SEVERAL ADDRESSES BECOMES SEVERAL CHIPS, and it is
              // handled here rather than in onPaste because it is the same rule:
              // anything separated is finished, the tail is still being typed.
              if (/[,;\n]/.test(value)) {
                flush(value);
                return;
              }
              setText(value);
            }}
            onKeyDown={onKeyDown}
            onFocus={() => {
              if (suggestions.length > 0) setOpen(true);
            }}
            onBlur={() => {
              blurTimer.current = setTimeout(() => setOpen(false), 120);
              // A COMPLETE ADDRESS LEFT IN THE BOX BECOMES A CHIP ON BLUR. The
              // alternative is a recipient that looks typed and is not in the
              // message — the failure this whole component exists to make
              // impossible. An INCOMPLETE one is left alone: chipping `ada@` would
              // destroy what someone was halfway through writing.
              const value = text.trim();
              if (looksLikeAddress(value)) flush(`${value},`);
            }}
          />
          {open && suggestions.length > 0 ? (
            <ul className="recipient-suggestions" id={listId} role="listbox" aria-label={label}>
              {suggestions.map((c, i) => {
                const addr = c.addresses[0]?.address ?? "";
                return (
                  <li key={c.id}>
                    <button
                      type="button"
                      role="option"
                      id={`${listId}-${i}`}
                      aria-selected={i === active}
                      className={`recipient-option ${i === active ? "is-active" : ""}`}
                      onMouseEnter={() => setActive(i)}
                      // mousedown, not click: fires before the input's blur so the
                      // selection lands, and preventDefault keeps focus put.
                      onMouseDown={(ev) => {
                        ev.preventDefault();
                        chooseContact(c);
                      }}
                    >
                      {c.displayName ? (
                        <span className="recipient-name">{c.displayName}</span>
                      ) : null}
                      <span className="recipient-address">{addr}</span>
                    </button>
                  </li>
                );
              })}
            </ul>
          ) : null}
        </span>
      </div>
    </div>
  );
}
