// SPDX-License-Identifier: AGPL-3.0-only

// COMPOSE (#448, design pass-2 §7) — reshaped from the docked bottom-right panel
// into the reading-pane column, with four things stated instead of implied.
//
// WHAT CHANGED, AND WHY EACH ONE IS THE POINT OF THE ISSUE:
//
//   1. WHERE IT IS. It was `position: fixed` in the corner over whatever you were
//      reading. It now takes the grid track ThreadPane uses, with the list still
//      beside it — because composing a reply is the same act as reading one, in
//      the same place. ./compose/placement.ts decides which of the three
//      positions applies; ComposeRoute passes it in.
//   2. THE FROM IS A SENTENCE. A <select> of every identity plus an editable
//      address box plus a hint became `house@example.com · because you are
//      replying to mail sent there`, with **Change** for the rest
//      (./compose/from.ts + FromLine.tsx).
//   3. RECIPIENTS ARE CHIPS THAT CARRY THE KEY FACT. `⚿ key held` / `no key` /
//      nothing, per recipient — which is unsayable about a substring of a
//      comma-separated string, and is the reason the field is chips at all
//      (./compose/chips.ts + RecipientChips.tsx).
//   4. THE SEND LINE. One sentence saying what will happen cryptographically, and
//      never a silent downgrade (./compose/sendLine.ts + SendLine.tsx).
//
// AND THE SEND-QUOTA METER IS GONE (#448, a deliberate decision). The client
// method stays (`client.sendQuotaGet`), but ⚠ NOTHING CALLS IT any more — the
// account-health panel gets its own send-quota numbers from `Account/getHealth`'s
// `sendQuota` field (AccountHealthPanel.tsx), not from this method. What left is
// the "N of M sends left today" line and its two strings. It was a limit
// restated on every screen where mail is written, about a cap that is enforced
// server-side either way.
//
// AUTOSAVE, AND THE SERVER FACT UNDER IT. A JMAP Email is immutable apart from
// keywords and mailboxIds, so a draft is EDITED by creating the new version and
// destroying the old one IN THE SAME `Email/set` (internal/api/draft.go →
// store.RetireDraft). `MailActions.saveDraft` is that call; ./compose/autosave.ts
// is the decision of when to make it. A failed save is reported in the FOOTER,
// not as a toast: it is a fact about this panel, and a toast every fifteen seconds
// behind a refusing server would be a machine gun.
//
// The identity story (spec §5) is unchanged and still the interesting part:
//
//   * The From address is a SECURITY BOUNDARY: the server rebuilds the RFC 5322
//     bytes itself and refuses any From not covered by a verified identity. The
//     failure comes back as a `forbiddenFrom` SetError and is shown verbatim.
//   * A catch-all account has a WILDCARD identity (`*@example.com`), so choosing
//     one keeps the "type the local part" box.
//   * A REPLY defaults From to the address the message was delivered to.
//   * A NEW message to a known contact HINTS the From from `Contact.reachedMeAt`
//     — the alias model run backwards — and the hint yields the moment the user
//     touches the From themselves.
//
// RICH TEXT (#46) and ATTACHMENTS (#85) are as they were: a dependency-free
// contentEditable with an execCommand toolbar and a plain-text toggle, and an
// upload-on-choose that puts a blobId in the draft create rather than bytes in the
// JSON. Forward adds one thing to that: a part of the parent travels by REFERENCE
// (its part blobId), which internal/api/draft.go resolveAttachments accepts
// through the same ownership gate the download route uses.

import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import type { JSX } from "react";

import type { EffectiveEmail } from "../../core/store";
import type {
  BlobId,
  ComposeSnippet,
  Contact,
  ContactAddress,
  EmailAddress,
  Id,
  Identity,
} from "../../core/jmap/types";
import { CAP_PGP } from "../../core/jmap/types";
import { formatWake } from "../../core/snooze";
import { markdownToHtml } from "../../core/markdown";
import { automaticSignatureBody } from "../../core/snippets";
import { useApp, useMailState } from "../state";
import { htmlToRender } from "./bodyChoice";
import { ImportKeyDialog } from "./ImportKeyDialog";
import { SendLaterMenu } from "./SendLaterMenu";
import { SnippetPicker } from "./SnippetPicker";
import { isWithheld, scanStateFor, withheldReasonKey } from "./attachmentScanPolicy";
import { focusMessageListSoon, trapTab } from "./focusTrap";
import { t, type MessageKey } from "../i18n";
import { AttachmentTiles, sendableAttachments } from "./compose/AttachmentTiles";
import type { Tile } from "./compose/AttachmentTiles";
import { FromLine } from "./compose/FromLine";
import { RecipientChips } from "./compose/RecipientChips";
import { SendStatusLine } from "./compose/SendStatusLine";
import { willNotSignReason, willSign } from "./compose/sendLine";
import { EncryptSwitch } from "./compose/EncryptSwitch";
import { addressesToLookUp, encryptOffer } from "./compose/encryptOffer";
import type { RecipientKeyInfo } from "../../core/jmap/types";
import { heldKeyForAddress, refreshOutcome } from "../../core/keyLifecycle";
import type { ComposeRefreshResult } from "../../core/keyLifecycle";
import { keyOutcome } from "../../core/userKeys";
import { chipKeyFact, chipKeyFactFromLookup, chipLabel } from "./compose/chips";
import type { Chip, ChipKeyFact } from "./compose/chips";
import {
  aliasNote,
  defaultIdentity,
  primaryIdentity,
  resumedIdentity,
  wildcardDomain,
} from "./compose/from";
import { aliasDisclosure } from "./compose/aliasDisclosure";
import { autosaveDecision, draftDirty, draftEmpty } from "./compose/autosave";
import type { DraftSnapshot } from "./compose/autosave";
import { trapsFocus } from "./compose/placement";
import type { ComposePlacement } from "./compose/placement";
import { initialReplyFields } from "./compose/replySeed";
import {
  archiveOnSuccessFor,
  offersSendAndArchive,
  paneToLeaveAfterSend,
  parentInInbox,
  queuedToastKey,
  sentToastKey,
} from "./compose/sendArchive";
import { requestPaneLeave } from "./useListVerbs";
import { currentRoute } from "../routing";
import { pickSignatureFor } from "./compose/signature";

/**
 * What the composer was opened to do.
 *
 * `forward` and `draft` are #448's additions. `forward` existed as a ROUTE with
 * nothing behind it (core/routes.ts `ComposeRef`) and now has a composer; `draft`
 * resumes a draft the server holds, which is how the Drafts list opens a row.
 *
 * `replyAll` is #646's addition: the same parent as `reply`, addressed
 * differently — see ./compose/recipients.ts for the decision of who that is.
 */
export type ComposeIntent =
  | { kind: "new" }
  | { kind: "reply"; parent: EffectiveEmail }
  | { kind: "replyAll"; parent: EffectiveEmail }
  | { kind: "forward"; parent: EffectiveEmail }
  | { kind: "draft"; draft: EffectiveEmail };

interface Props {
  intent: ComposeIntent;
  /** Where this composer sits, which decides whether focus is trapped
   * (./compose/placement.ts). */
  placement: ComposePlacement;
  /** The account the surrounding list is scoped to, or null for every account —
   * what "your primary address" is measured in. */
  scope: Id | null;
  onClose: () => void;
  /** Told the id of every draft revision an autosave lands, so the route can
   * move its URL to it without remounting this composer (#612). */
  onDraftSaved?: (draftId: Id) => void;
}

/** How often the autosave scheduler asks whether to save. Shorter than the
 * interval it enforces, so "15 s since the last save" is answered within a few
 * seconds of being true rather than up to fifteen seconds late. */
const AUTOSAVE_TICK_MS = 3_000;
/** The interval autosaveDecision enforces (design pass-2 §7: every 15 s). */
const AUTOSAVE_INTERVAL_MS = 15_000;

/** A file the user attached: uploaded (so it has a blobId) and ready to be named
 * in the Email/set create. */
interface Attached {
  blobId: BlobId;
  name: string;
  type: string;
  size: number;
}

/** An image embedded in the HTML body: uploaded like any attachment, but carried
 * inline and referenced from the body as `<img src="cid:${cid}">`. Kept OUT of
 * the visible tile list so it is not double-listed. */
interface InlineImage {
  blobId: BlobId;
  cid: string;
  name: string;
  type: string;
}

/** How the footer describes the draft's state. */
type SaveState =
  | { kind: "never" }
  | { kind: "saving" }
  | { kind: "saved" }
  | { kind: "failed"; reason: string };

/** Escapes text for safe interpolation into HTML the editor will show. (The
 * SERVER is the security boundary for what is SENT; this keeps user text from
 * accidentally becoming markup inside the editor's own document.) */
function escapeHtml(s: string): string {
  return s
    .replace(/&/g, "&amp;")
    .replace(/</g, "&lt;")
    .replace(/>/g, "&gt;")
    .replace(/"/g, "&quot;");
}

/** Plain text → HTML, for the plain→rich toggle: paragraphs on blank lines, soft
 * breaks on single newlines. */
function textToHtml(text: string): string {
  if (text.trim() === "") return "";
  return text
    .split(/\n{2,}/)
    .map((para) => `<p>${escapeHtml(para).replace(/\n/g, "<br>")}</p>`)
    .join("");
}

/** HTML → plain text. Used both for the multipart/alternative text fallback and
 * for the rich→plain toggle. Block elements become line breaks, list items get a
 * bullet, a blockquote's lines are prefixed with "> ", and an image collapses to
 * its alt text — a readable plain rendering, not a perfect one. */
function htmlToText(html: string): string {
  const tpl = document.createElement("template");
  tpl.innerHTML = html; // trusted-html: inert <template>, no rendering/execution (ADR-015)
  return walkText(tpl.content)
    .replace(/[ \t]+\n/g, "\n")
    .replace(/\n{3,}/g, "\n\n")
    .trim();
}

function walkText(node: Node): string {
  let out = "";
  node.childNodes.forEach((child) => {
    if (child.nodeType === Node.TEXT_NODE) {
      out += (child.textContent ?? "").replace(/\s+/g, " ");
      return;
    }
    if (child.nodeType !== Node.ELEMENT_NODE) return;
    const el = child as HTMLElement;
    const tag = el.tagName.toLowerCase();
    if (tag === "br") {
      out += "\n";
      return;
    }
    if (tag === "img") {
      const alt = el.getAttribute("alt");
      out += alt ? `[${alt}]` : "[image]";
      return;
    }
    const inner = walkText(el);
    if (tag === "blockquote") {
      out += `${inner
        .split("\n")
        .map((line) => `> ${line}`)
        .join("\n")}\n`;
    } else if (tag === "li") {
      out += `• ${inner}\n`;
    } else if (/^(p|div|h[1-6]|ul|ol|tr|table|section|article|header|footer|figure|pre)$/.test(tag)) {
      out += `${inner}\n`;
    } else {
      out += inner;
    }
  });
  return out;
}

/** A fresh Content-ID for an inline image. It is used both in the body's
 * `<img src="cid:…">` and as the attachment's `cid`, and the server writes it
 * into the part's `Content-ID` header — that is what lets a recipient's client
 * resolve the reference. Unpredictable and unique per image. */
function newCid(): string {
  const rand =
    typeof crypto !== "undefined" && typeof crypto.randomUUID === "function"
      ? crypto.randomUUID()
      : `${Date.now().toString(36)}-${Math.random().toString(36).slice(2)}`;
  return `img-${rand}@inline.mailroom`;
}

/** A message's decoded plain text, when its body values have been fetched. */
function plainTextOf(email: EffectiveEmail | null): string {
  if (!email) return "";
  const values = email.bodyValues ?? {};
  return (email.textBody ?? [])
    .map((p) => (p.partId ? values[p.partId]?.value ?? "" : ""))
    .filter(Boolean)
    .join("\n\n");
}

/** A message's decoded HTML body, when it has one and it has been fetched. */
function htmlBodyOf(email: EffectiveEmail | null): string {
  if (!email) return "";
  // "" for a plain-only message (#871): its lone text/plain part is listed in
  // htmlBody too, and treating that text as HTML both opened a plain draft in
  // rich mode and collapsed its line breaks.
  return htmlToRender(email, "");
}

/** The attribution line above a quote. */
function quoteHeader(parent: EffectiveEmail): string {
  const who = parent.from?.[0]?.name || parent.from?.[0]?.email || "";
  return t("compose.quoteHeader", { who, date: new Date(parent.receivedAt).toLocaleString() });
}

/** The plain-text quoted body of the message being replied to or forwarded, when
 * its text has been fetched (Email.bodyValues, populated by the reading pane's
 * Email/get). Quoting nothing is deliberate when it has not: a quote is a
 * convenience, and inventing one from the preview would put words in the user's
 * mouth. */
function quotedText(parent: EffectiveEmail | null): string {
  if (!parent) return "";
  const text = plainTextOf(parent);
  if (!text) return "";
  return `\n\n${quoteHeader(parent)}\n${text
    .split("\n")
    .map((l) => `> ${l}`)
    .join("\n")}\n`;
}

/** The same quote as HTML: an empty line to type above, the attribution, then the
 * parent's text in a <blockquote>. Built from the parent's PLAIN text (escaped)
 * rather than its HTML — we quote what we are sure of. */
function quotedHtml(parent: EffectiveEmail | null): string {
  if (!parent) return "";
  const text = plainTextOf(parent);
  if (!text) return "";
  const bodyHtml = escapeHtml(text).replace(/\n/g, "<br>");
  return `<p><br></p><p>${escapeHtml(quoteHeader(parent))}</p><blockquote>${bodyHtml}</blockquote>`;
}

/** The dependency-free rich-text editor: a contentEditable surface plus a small
 * execCommand toolbar. It is UNCONTROLLED — the initial HTML is written to the
 * DOM once (writing innerHTML on every render would slam the caret to the top on
 * each keystroke) and every edit is read back out through `onChange`. */
function RichEditor({
  initialHTML,
  onChange,
  onImageFile,
  onBlur,
  label,
}: {
  initialHTML: string;
  onChange: (html: string) => void;
  onImageFile: (file: File) => Promise<string | null>;
  onBlur?: () => void;
  label: string;
}): JSX.Element {
  const ref = useRef<HTMLDivElement>(null);
  const imgRef = useRef<HTMLInputElement>(null);
  const savedRange = useRef<Range | null>(null);

  useEffect(() => {
    if (ref.current) ref.current.innerHTML = initialHTML; // trusted-html: escaped plain-text reply quote only (ADR-015)
    // initialHTML is the mount-time seed only; onChange is stable enough for our
    // use and re-seeding on either would fight the uncontrolled editor. The
    // composer re-KEYS this component when the seed legitimately changes (a From
    // change while the body is untouched), which is a remount rather than a
    // re-seed.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  const sync = (): void => {
    if (ref.current) onChange(ref.current.innerHTML); // trusted-html: reads user's own authored content, re-sanitized server-side on send (ADR-015)
  };

  // SECURITY (ADR-015): paste as PLAIN TEXT. Pasting rich HTML would inject
  // attacker-controlled markup (e.g. copied from a malicious page) straight into
  // our live editable DOM, where an <img onerror> fires — precisely the vector
  // the no-innerHTML invariant guards against. Rich paste would need a
  // client-side sanitizer we deliberately don't ship; plain-text paste closes it.
  const pasteAsText = (e: React.ClipboardEvent<HTMLDivElement>): void => {
    e.preventDefault();
    const text = e.clipboardData.getData("text/plain");
    document.execCommand("insertText", false, text);
    sync();
  };

  // Save/restore the caret across actions that steal focus (a link prompt, the
  // image file dialog): without it, insertHTML/createLink would land nowhere.
  const saveSelection = (): void => {
    const sel = window.getSelection();
    if (sel && sel.rangeCount > 0 && ref.current?.contains(sel.anchorNode)) {
      savedRange.current = sel.getRangeAt(0).cloneRange();
    }
  };
  const restoreSelection = (): void => {
    const el = ref.current;
    if (!el) return;
    el.focus();
    const r = savedRange.current;
    if (r) {
      const sel = window.getSelection();
      sel?.removeAllRanges();
      sel?.addRange(r);
    }
  };

  // execCommand is deprecated but universally implemented and needs no library —
  // exactly the minimal, self-contained editor #46 asks for (no npm dep, no CSP
  // or bundle cost). The result is re-read via sync() and SERVER-sanitized before
  // it is transmitted, so an odd browser's output cannot become hostile bytes.
  const exec = (command: string, value?: string): void => {
    document.execCommand(command, false, value);
    sync();
  };

  const addLink = (): void => {
    const url = window.prompt(t("compose.linkPrompt"));
    restoreSelection();
    if (url && url.trim() !== "") document.execCommand("createLink", false, url.trim());
    sync();
  };

  const onImageChosen = async (files: FileList | null): Promise<void> => {
    const file = files?.[0];
    if (imgRef.current) imgRef.current.value = "";
    if (!file) return;
    const cid = await onImageFile(file);
    if (!cid) return;
    restoreSelection();
    document.execCommand(
      "insertHTML",
      false,
      `<img src="cid:${cid}" alt="${escapeHtml(file.name)}" style="max-width:100%">`,
    );
    sync();
  };

  // A formatting button that acts on the LIVE selection: mousedown+preventDefault
  // keeps the editor focused (a plain onClick would blur it first).
  const tool = (cmd: string, labelKey: MessageKey, glyph: string, cssGlyph = false): JSX.Element => (
    <button
      type="button"
      className="editor-tool"
      aria-label={t(labelKey)}
      title={t(labelKey)}
      data-glyph={cssGlyph ? glyph : undefined}
      onMouseDown={(ev) => {
        ev.preventDefault();
        exec(cmd);
      }}
    >
      {cssGlyph ? null : glyph}
    </button>
  );

  // "1." is TEXT to axe (WCAG 2.5.3, label-content-name-mismatch in axe-core
  // 4.14): a name of "Numbered list" does not contain it. Painted from CSS
  // (.editor-tool[data-glyph]::before) it is decoration, not label content.

  return (
    <div className="editor">
      <div className="editor-toolbar" role="toolbar" aria-label={t("compose.formatting")}>
        {tool("bold", "compose.bold", "B")}
        {tool("italic", "compose.italic", "I")}
        {tool("underline", "compose.underline", "U")}
        {tool("insertUnorderedList", "compose.bulletList", "•")}
        {tool("insertOrderedList", "compose.numberList", "1.", true)}
        <button
          type="button"
          className="editor-tool"
          aria-label={t("compose.link")}
          title={t("compose.link")}
          // Save the caret while it is still in the editor; the prompt blurs it.
          onMouseDown={(ev) => {
            ev.preventDefault();
            saveSelection();
          }}
          onClick={addLink}
        >
          🔗
        </button>
        <button
          type="button"
          className="editor-tool"
          aria-label={t("compose.insertImage")}
          title={t("compose.insertImage")}
          onMouseDown={(ev) => {
            ev.preventDefault();
            saveSelection();
          }}
          onClick={() => imgRef.current?.click()}
        >
          🖼
        </button>
      </div>
      <div
        ref={ref}
        className="editor-surface"
        contentEditable
        suppressContentEditableWarning
        role="textbox"
        aria-multiline="true"
        aria-label={label}
        onInput={sync}
        onBlur={() => {
          saveSelection();
          onBlur?.();
        }}
        onPaste={pasteAsText}
      />
      <input
        ref={imgRef}
        type="file"
        accept="image/*"
        className="editor-image-input"
        aria-hidden="true"
        tabIndex={-1}
        onChange={(ev) => void onImageChosen(ev.target.files)}
      />
    </div>
  );
}

/** The text/html actually spliced in for a signature SOURCE (a snippet's own
 * body, or `automaticSignatureBody`'s) — falling back html<->text, so a
 * signature saved in only one form still appears in both bodies compose
 * keeps. Factored out of `openingBodies` so a signature picked BY HAND (#649,
 * `applyPickedSignature` below) computes and tracks the exact same two
 * strings, rather than a second copy of this fallback drifting from it. */
function signatureBlockStrings(
  sig: { html: string; text: string } | null,
): { text: string; html: string } | null {
  if (!sig) return null;
  return { text: sig.text || htmlToText(sig.html), html: sig.html || textToHtml(sig.text) };
}

/** What compose starts with: room to type, then the default signature, then the
 * quote. That ORDER is the convention — the quote stays last, where a reader looks
 * for it, and the signature sits below what you write rather than under the quoted
 * mail. */
function openingBodies(
  parent: EffectiveEmail | null,
  sig: { html: string; text: string } | null,
): { text: string; html: string } {
  const quoteText = quotedText(parent);
  const quoteHtml = quotedHtml(parent);
  const block = signatureBlockStrings(sig);
  if (!block) return { text: quoteText, html: quoteHtml };
  return {
    text: `\n\n${block.text}${quoteText}`,
    // The quote already opens with a blank paragraph to type in; the signature
    // goes after that one rather than adding a second.
    html: `<p><br></p>${block.html}${quoteHtml.replace(/^<p><br><\/p>/, "")}`,
  };
}

/** The chips a resumed draft or a reply starts with. */
function chipsFrom(addrs: EmailAddress[] | null | undefined): Chip[] {
  return (addrs ?? []).map((a) => ({ address: a.email, name: a.name ?? null }));
}

/**
 * WHAT TRAVELS WITH THE MESSAGE: the tiles that may be sent, plus the inline
 * images the HTML being sent still references.
 *
 * ONE FUNCTION FOR BOTH THE SEND AND THE AUTOSAVE, because they must agree: a
 * draft saved without its inline images would come back, when resumed, as a body
 * full of broken `cid:` references.
 *
 * The inline filter is against the HTML BEING SENT, not the editor's own
 * `htmlBody`: in Markdown mode those are different documents, and the stale one
 * would smuggle an orphan part for an image no longer referenced anywhere.
 */
function outgoingAttachments(
  tiles: readonly Tile[],
  inlineImages: readonly InlineImage[],
  htmlOut: string,
): {
  blobId: BlobId;
  type: string;
  name: string;
  cid?: string;
  disposition?: "inline";
}[] {
  const inline = htmlOut
    ? inlineImages
        .filter((im) => htmlOut.includes(`cid:${im.cid}`))
        .map((im) => ({
          blobId: im.blobId,
          type: im.type || "application/octet-stream",
          name: im.name,
          cid: im.cid,
          disposition: "inline" as const,
        }))
    : [];
  return [...sendableAttachments(tiles), ...inline];
}

/** A chip as the JMAP EmailAddress a draft create and a send both take. */
function asAddress(chip: Chip): EmailAddress {
  return { name: chip.name, email: chip.address };
}

/** `Fwd: x` / `Re: x`, added only when it is not already there. */
function prefixedSubject(subject: string, prefix: "Re" | "Fwd"): string {
  const re = prefix === "Re" ? /^re:/i : /^fwd?:/i;
  return re.test(subject) ? subject : `${prefix}: ${subject}`;
}

export function Compose({ intent, placement, scope, onClose, onDraftSaved }: Props): JSX.Element {
  const state = useMailState();
  const { actions, client, pushToast, offerUndo } = useApp();
  const panelRef = useRef<HTMLDivElement>(null);

  /**
   * WHETHER THE USER HAS EDITED ANYTHING (#614). Autosave waits for it
   * (compose/autosave.ts): a reply or forward opens non-empty, and without this an
   * untouched one was saved as a draft within one tick of opening.
   *
   * ⚠ ONLY THE USER'S OWN ACTIONS MAY SET IT, never an effect: the composer changes
   * itself after mount (the signature is re-seeded when From resolves, the alias
   * hint can move From), and counting that as an edit would bring the bug back.
   * Set two ways, deliberately redundant:
   *   - a DOM net on the panel root (below): every `input`, `change`, `paste` and
   *     `drop` inside the composer. Only a real user action fires those — a value
   *     set by code fires nothing — so the net cannot misfire;
   *   - explicitly, in each edit that fires NONE of them: a recipient chip removed
   *     by click or Backspace, an attachment removed or released, From chosen from
   *     the list, rich/plain toggled, a snippet inserted, a file attached.
   * Missing a path would drop that edit when the composer closed; marking one too
   * many costs a save. So when in doubt, mark.
   */
  const touchedRef = useRef(false);
  const markTouched = useCallback((): void => {
    touchedRef.current = true;
  }, []);
  useEffect(() => {
    const root = panelRef.current;
    if (!root) return;
    const kinds = ["input", "change", "paste", "drop"] as const;
    for (const k of kinds) root.addEventListener(k, markTouched, true);
    return () => {
      for (const k of kinds) root.removeEventListener(k, markTouched, true);
    };
  }, [markTouched]);

  /**
   * Every identity in scope, in the SERVER'S order — disabled ones included.
   *
   * Disabled ones are here because the **Change** list prints them (with
   * `disabled` on the row), which is how "why can I not send from that address?"
   * gets answered. The order is the store's, which is `Identity/get`'s: "primary"
   * is a POSITION, not a flag (there is no flag on Identity — ./compose/from.ts
   * says so), so re-sorting here would make it mean something else.
   */
  const identities = useMemo(
    () => Object.values(state.identities).filter((i) => !scope || i.accountId === scope),
    [state.identities, scope],
  );

  /**
   * EVERY identity in the store, not just the ones the surrounding list is
   * scoped to (#646). `identities` above is scoped because the From list must
   * only offer addresses the user can send from HERE; but "is this my own
   * address" (recipients.ts isOwnAddress, for stripping the user out of a
   * reply-all) is a question about every account the user holds, not the one
   * being viewed — mail to the user's alias on another account is still
   * theirs, and a wildcard identity on that other account makes every local
   * part at its domain theirs too.
   */
  const allIdentities = useMemo(() => Object.values(state.identities), [state.identities]);

  const parent =
    intent.kind === "reply" || intent.kind === "replyAll" || intent.kind === "forward"
      ? intent.parent
      : null;
  // Send & archive is offered only for a parent that is IN THE INBOX (#648;
  // ./compose/sendArchive.ts says why): computed once for the button and the
  // Ctrl/Cmd+Shift+Enter shortcut, so the two can never disagree.
  const canSendAndArchive = offersSendAndArchive(intent.kind, parentInInbox(parent, state.mailboxes));
  const resumed = intent.kind === "draft" ? intent.draft : null;
  const forwarding = intent.kind === "forward" ? intent.parent : null;

  const parentFacts = useMemo(
    () =>
      parent
        ? { identityId: parent.identityId ?? null, deliveredTo: parent.deliveredTo ?? null }
        : null,
    [parent],
  );

  /** The identity compose opens on (./compose/from.ts). For a RESUMED draft it is
   * the one the draft's own From resolved to, so continuing a draft does not
   * silently change who it is from. */
  const initialIdentity = useMemo(() => {
    if (resumed) {
      // ⚠ A DERIVED ALIAS IS NOT A CONCRETE IDENTITY (#599). This used to match
      // only non-wildcard identities, so on a catch-all a draft saved as
      // `mortgage@…` matched nothing and fell through to the primary — silently
      // changing who the draft was from, which is exactly what the comment
      // above says must not happen. resumedIdentity resolves the wildcard that
      // owns the address's domain too.
      const match = resumedIdentity(identities, resumed.from?.[0]?.email);
      if (match) return match;
    }
    return defaultIdentity(identities, parentFacts);
  }, [resumed, identities, parentFacts]);

  const [identityId, setIdentityId] = useState<string>(initialIdentity?.id ?? "");
  const identity: Identity | undefined = state.identities[identityId];
  const primary = useMemo(() => primaryIdentity(identities), [identities]);

  /** The address typed into a catch-all's local-part box. Seeded from the parent's
   * `deliveredTo` (or the resumed draft's own From) so a reply through a catch-all
   * comes from the address they actually wrote to. */
  const [localPart, setLocalPart] = useState(() => {
    const seed = resumed?.from?.[0]?.email ?? parent?.deliveredTo ?? "";
    return initialIdentity?.isWildcard ? seed : "";
  });

  /**
   * The reply/reply-all target (#646), computed exactly ONCE — `to`'s and
   * `cc`'s own initializers below used to each decide their half of this
   * separately (only `to` even existed before reply-all), which is the same
   * question asked twice. `initialReplyFields` (./compose/replySeed.ts) is the
   * empty seed for every other intent, so neither field needs a branch of its
   * own for "is this even a reply".
   */
  const replySeed = useMemo(
    () =>
      initialReplyFields(
        intent.kind === "reply" || intent.kind === "replyAll" ? intent.kind : null,
        parent,
        allIdentities,
      ),
    [intent, parent, allIdentities],
  );

  const [to, setTo] = useState<Chip[]>(() => (resumed ? chipsFrom(resumed.to) : replySeed.to));
  const [cc, setCc] = useState<Chip[]>(() => (resumed ? chipsFrom(resumed.cc) : replySeed.cc));
  const [bcc, setBcc] = useState<Chip[]>(() => (resumed ? chipsFrom(resumed.bcc) : []));
  const [ccOpen, setCcOpen] = useState(cc.length > 0);
  const [bccOpen, setBccOpen] = useState(bcc.length > 0);

  const [subject, setSubject] = useState(() => {
    if (resumed) return resumed.subject ?? "";
    if (intent.kind === "reply" || intent.kind === "replyAll") {
      return prefixedSubject(intent.parent.subject, "Re");
    }
    if (intent.kind === "forward") return prefixedSubject(intent.parent.subject, "Fwd");
    return "";
  });

  // Saved signatures and templates (#56). Read BEFORE the body state, because the
  // default signature is part of what the body starts as.
  const snippets = useMemo(() => Object.values(state.composeSnippets), [state.composeSnippets]);
  const [snippetPicker, setSnippetPicker] = useState<"template" | "signature" | null>(null);
  const hasTemplates = snippets.some((s) => s.kind === "template");
  // Any saved signature (#649) — offered only when there is at least one, same
  // as the template button, so an empty picker never opens.
  const hasSignatures = snippets.some((s) => s.kind === "signature");

  const [richMode, setRichMode] = useState(() => (resumed ? htmlBodyOf(resumed) !== "" : true));
  // Markdown compose (#56): an option on plain mode. Off by default — someone who
  // has not asked for it must not have their asterisks reinterpreted.
  const [markdown, setMarkdown] = useState(false);

  /**
   * The body compose PUT THERE, and the memory is what makes "have you started
   * writing?" answerable: a body still equal to this is untouched, so a template
   * may fill it and a From change may re-seed the signature. Without it the
   * auto-signature would itself count as typing.
   *
   * ⚠ A RESUMED DRAFT'S OPENING IS ITS OWN BODY, so `untouched` there means
   * "unedited since it was resumed" — which is NOT what the template picker's rule
   * asks ("before you start writing"): the user already wrote it, possibly days
   * ago. So the picker is passed `untouched && not a resumed draft`, and the two
   * meanings are kept apart rather than sharing one boolean.
   */
  const opening = useRef(
    resumed
      ? { text: plainTextOf(resumed), html: htmlBodyOf(resumed) }
      : openingBodies(parent, automaticSignatureBody(snippets, initialIdentity)),
  );
  const [body, setBody] = useState(opening.current.text); // plain-mode text
  const [htmlBody, setHtmlBody] = useState(opening.current.html); // rich-mode html

  /**
   * THE SIGNATURE BLOCK CURRENTLY EMBEDDED in the body (#649) — compose's own
   * initial pick to start, then whatever the From-reseed effect below or a
   * signature picked BY HAND (`applyPickedSignature`) last put there. Compared
   * against on every manual pick (./compose/signature.ts `pickSignatureFor`)
   * to decide replace vs insert, so a SECOND manual pick compares against what
   * the FIRST one left rather than the original default.
   *
   * `null` for a RESUMED draft: its body is the user's own past work from a
   * previous session, not something THIS composer inserted, so there is no
   * block of compose's own to find and replace — a manual pick there always
   * inserts, same as everywhere else with no current block.
   */
  const currentSignature = useRef(
    resumed ? null : signatureBlockStrings(automaticSignatureBody(snippets, initialIdentity)),
  );
  /** Bumped to REMOUNT the uncontrolled rich editor when its seed legitimately
   * changes (see the From effect). */
  const [editorGeneration, setEditorGeneration] = useState(0);

  const untouched =
    (richMode ? htmlBody : body).trim() === "" ||
    (richMode ? htmlBody === opening.current.html : body === opening.current.text);

  const [sending, setSending] = useState(false);
  const [sendLaterOpen, setSendLaterOpen] = useState(false);

  // The server's advertised scheduling horizon, read from the session rather than
  // assumed: the server ENFORCES it, so offering a preset it would refuse would be
  // offering a choice that fails. null means the session did not say it, in which
  // case every preset is offered and the server has the final word anyway.
  const maxDelayedSendSeconds = useMemo(() => {
    const cap = state.session?.capabilities?.["urn:ietf:params:jmap:submission"];
    const v = (cap as { maxDelayedSend?: unknown } | undefined)?.maxDelayedSend;
    return typeof v === "number" && Number.isFinite(v) ? v : null;
  }, [state.session]);

  const [attached, setAttached] = useState<Attached[]>(() =>
    // A RESUMED DRAFT'S ATTACHMENTS ARE ALREADY ON THE SERVER, so they come back
    // as blob references rather than as uploads to repeat — the same by-reference
    // path a forward uses.
    //
    // ⚠ AN INLINE IMAGE IS NOT ONE OF THESE. A part with a `cid` is referenced from
    // the HTML body as `<img src="cid:…">`; listing it here would show it twice (in
    // the body and as a file) and — worse — re-attach it WITHOUT its cid and
    // disposition, which breaks the reference in a body that still names it. It goes
    // to `inlineImages` below instead.
    (resumed?.attachments ?? [])
      .filter((part) => part.blobId && !part.cid)
      .map((part) => ({
        blobId: part.blobId as BlobId,
        name: part.name ?? part.type,
        type: part.type,
        size: part.size,
      })),
  );
  const [inlineImages, setInlineImages] = useState<InlineImage[]>(() =>
    (resumed?.attachments ?? [])
      .filter((part) => part.blobId && part.cid)
      .map((part) => ({
        blobId: part.blobId as BlobId,
        cid: part.cid as string,
        name: part.name ?? "image",
        type: part.type,
      })),
  );
  const [uploading, setUploading] = useState(false);
  /**
   * Parts of the forwarded message this session has RELEASED.
   *
   * ⚠ IT HAS TO BE LOCAL, because the parent is FROZEN. ComposeRoute captures the
   * message as it was when the composer opened, on purpose (a quoted parent that
   * changed mid-draft would be a different reply), so a release — which changes the
   * scan state server-side — can never reach `intent.parent`. The set is only
   * added to after `actions.releaseAttachment` RESOLVED TRUE, i.e. after the server
   * said yes, so it records a fact rather than an intention.
   */
  const [released, setReleased] = useState<Set<string>>(() => new Set());

  /** The alias the From was hinted to, and whether the user has taken the From
   * control into their own hands (which ends the hinting). */
  const [aliasHint, setAliasHint] = useState<string | null>(null);
  const [fromOverridden, setFromOverridden] = useState(false);

  // -- keys -------------------------------------------------------------------

  const keysAvailable = client.hasCapability(CAP_PGP);

  /**
   * Whether this send will be signed — read from the server's OWN facts
   * (#492), not a client guess.
   *
   * `identity.signingKeyId` (whether a key is bound to THIS identity) and
   * `client.pgpSigning()` (whether THIS SERVER's crypto process is wired at
   * all) are the two halves; `willSign` (ui/compose/sendLine.ts) is the pure
   * function that combines them. Both are synchronous — the identity comes
   * from already-loaded store state and the signing fact comes from the
   * session that was fetched before this composer could open — so there is no
   * "not back yet" state to render around, unlike the old `UserKey/get`-based
   * reading this replaced.
   */
  const signing = client.pgpSigning();
  const ownKey: boolean | null = willSign(identity, signing);
  const ownKeyReason = willNotSignReason(identity, signing);

  /**
   * What is known about each recipient address, keyed lower-cased.
   *
   * ONE CACHE FOR THE CHIPS AND THE SEND LINE. They print the same fact in two
   * places, and two caches would be two answers to one question — which is
   * exactly the kind of disagreement CLAUDE.md's measurement rules are about.
   *
   * A key that is absent, or present as `undefined`, is "unknown"; `null` is "the
   * server answered and holds no contact for this address", which IS "no key
   * held" (core/jmap/client.ts contactForAddress).
   */
  const [contacts, setContacts] = useState<Record<string, Contact | null>>({});
  /** Addresses already asked about, so a chip removed and re-added is not a second
   * round trip and an in-flight lookup is not duplicated. */
  const asked = useRef<Set<string>>(new Set());

  const needFact = useCallback(
    (address: string, known?: Contact): void => {
      const key = address.trim().toLowerCase();
      if (key === "") return;
      if (known) {
        // The pick already IS the answer; resolving it again would be a round trip
        // for data in hand.
        asked.current.add(key);
        setContacts((cur) => ({ ...cur, [key]: known }));
        return;
      }
      if (asked.current.has(key)) return;
      asked.current.add(key);
      void client.contactForAddress(address).then((c) => {
        // `undefined` — could not ask — is deliberately NOT stored: leaving the key
        // absent keeps it "unknown" and lets a later attempt answer it, whereas
        // storing anything would freeze a non-answer into the cache.
        if (c === undefined) {
          asked.current.delete(key);
          return;
        }
        setContacts((cur) => ({ ...cur, [key]: c }));
      });
    },
    [client],
  );

  const factFor = useCallback(
    (address: string): ChipKeyFact => {
      const key = address.trim().toLowerCase();
      return chipKeyFact(key in contacts ? contacts[key] : undefined);
    },
    [contacts],
  );

  /** Every recipient, in the order the send line names them. */
  const allChips = useMemo(() => [...to, ...cc, ...bcc], [to, cc, bcc]);

  // A reply/forward/resume arrives with recipients already in place, so their key
  // facts have to be resolved without a keystroke — otherwise the send line would
  // read "unknown" until someone edited a field.
  useEffect(() => {
    for (const chip of allChips) needFact(chip.address);
  }, [allChips, needFact]);

  // -- the From address -------------------------------------------------------

  /** The address that goes in the From header. For a catch-all it is what the user
   * typed; for a concrete identity it is the identity's own. */
  const fromAddress = identity
    ? identity.isWildcard
      ? localPart.trim()
      : identity.email
    : "";

  // -- encryption (#47, ADR-038) -----------------------------------------------
  //
  // THE SWITCH IS OFFERED ONLY WHEN EVERY To/Cc RECIPIENT HAS A USABLE KEY, and
  // that is the server's answer (RecipientKey/lookup), not a guess from the chips'
  // "key held" facts: the server also applies expiry, revocation, "the key names
  // this address", authenticated-Autocrypt-only and the key-changed pin. Bcc is
  // never asked about and forbids the switch outright.
  //
  // `wantEncrypt` is the user's choice; `encrypting` is what is actually sent, and
  // it is false whenever the offer is withdrawn — a recipient added after the box
  // was ticked turns it off rather than leaving a send that will be refused.
  const [wantEncrypt, setWantEncrypt] = useState(false);
  const [recipientKeys, setRecipientKeys] = useState<Record<string, RecipientKeyInfo | undefined>>({});
  // The chips' key fact follows the same lookup the Encrypt line is built from
  // (#809); until it answers for an address the Contact-derived fact stands.
  const chipFactFor = useCallback(
    (address: string): ChipKeyFact =>
      chipKeyFactFromLookup(factFor(address), recipientKeys[address.trim().toLowerCase()]),
    [factFor, recipientKeys],
  );
  // The send line takes the same fact as the chips, so a lapsed key is not
  // counted under "keys held for" while the chip beside it says "key expired".
  const sendLineChips = useMemo(
    () =>
      allChips.map((chip) => ({
        name: chipLabel(chip),
        fact: keysAvailable ? chipFactFor(chip.address) : ("unknown" as ChipKeyFact),
      })),
    [allChips, chipFactFor, keysAvailable],
  );
  const serverCanEncrypt = client.pgpEncrypt();
  const lookupAddresses = useMemo(
    () => addressesToLookUp(to.map((c) => c.address), cc.map((c) => c.address), fromAddress),
    [to, cc, fromAddress],
  );
  const lookupKey = lookupAddresses.join(",");
  useEffect(() => {
    if (serverCanEncrypt !== true || ownKey !== true || lookupAddresses.length === 0) return;
    let cancelled = false;
    // Debounced: a lookup can reach out to a recipient's domain over WKD.
    const timer = setTimeout(() => {
      void client.recipientKeyLookup(lookupAddresses).then((list) => {
        if (cancelled || list === null) return;
        setRecipientKeys((cur) => {
          const next = { ...cur };
          for (const info of list) next[info.address.toLowerCase()] = info;
          return next;
        });
      });
    }, 500);
    return () => {
      cancelled = true;
      clearTimeout(timer);
    };
    // lookupKey stands for lookupAddresses: the array identity changes every render.
  }, [client, lookupKey, serverCanEncrypt, ownKey]);
  const offer = encryptOffer({
    serverCanEncrypt,
    ownKey,
    ownAddress: fromAddress,
    to: to.map((c) => c.address),
    cc: cc.map((c) => c.address),
    bcc: bcc.map((c) => c.address),
    keys: recipientKeys,
  });
  const encrypting = wantEncrypt && offer.offered;
  // The key-changed decision (#47, ADR-039), the same one the reading pane offers.
  // Once the server has it, ask again so the offer reflects the NEW state (the
  // changed key trusted and usable, or the old one kept) rather than ours.
  const refreshRecipientKeys = async (addresses: string[]): Promise<void> => {
    const list = await client.recipientKeyLookup(addresses.map((a) => a.toLowerCase()));
    if (list === null) return;
    setRecipientKeys((cur) => {
      const next = { ...cur };
      for (const info of list) next[info.address.toLowerCase()] = info;
      return next;
    });
  };
  const decideRecipientKey = async (
    address: string,
    fingerprint: string,
    decision: "trust" | "keep",
  ): Promise<boolean> => {
    const ok = await actions.decideContactKey(address, fingerprint, decision);
    if (!ok) return false;
    await refreshRecipientKeys([address]);
    return true;
  };
  // "Check for an updated key" (#818) from "Can't encrypt: X's key expired on …":
  // find the key held for the address, ask the server to look it up again over
  // WKD, then ask about the address again so the line follows what the server
  // now holds (a renewal offers the switch; a different key shows as changed).
  const refreshRecipientKey = async (address: string): Promise<ComposeRefreshResult> => {
    const held = await client.contactKeysRead();
    if (held === null) return { kind: "error", error: "unavailable" };
    const key = heldKeyForAddress(held.keys, held.contacts, address);
    if (!key) return { kind: "noKey" };
    const out = await keyOutcome(() => client.contactKeyRefresh(key.id));
    await refreshRecipientKeys([address]);
    return out.ok ? refreshOutcome(out.value) : { kind: "error", error: out.error };
  };
  // "Import their key..." (#47, ADR-038): the dialog for a recipient with no key.
  // Once the server has the key, ask again so the Encrypt switch is offered from
  // the server's answer, not from our guess that the import worked.
  const [importKeyFor, setImportKeyFor] = useState<string | null>(null);

  /**
   * A From change re-seeds the SIGNATURE — but only while the body is untouched.
   *
   * The signature belongs to the sending identity (core/snippets.ts
   * automaticSignatureBody: a signature written for shop@ beats a general one when
   * sending from shop@), so it has to follow a change of From. It does NOT rewrite
   * a body someone has written in: compose has no undo, and nothing here may
   * destroy text — the same rule the snippet picker states. The rich editor is
   * UNCONTROLLED, so re-seeding it means remounting it, which is what
   * `editorGeneration` is for.
   */
  const seededFor = useRef(identityId);
  useEffect(() => {
    if (seededFor.current === identityId) return;
    seededFor.current = identityId;
    const isUntouched =
      (richMode ? htmlBody : body).trim() === "" ||
      (richMode ? htmlBody === opening.current.html : body === opening.current.text);
    if (!isUntouched || resumed) return;
    const nextSig = automaticSignatureBody(snippets, identity);
    const next = openingBodies(parent, nextSig);
    opening.current = next;
    // A MANUAL PICK SURVIVES THIS (#649): reaching this line at all requires
    // `isUntouched`, i.e. the body still equals `opening.current` from before —
    // and a manual pick always changes the body to something else
    // (./compose/signature.ts's replace and insert both do), so a body a
    // manual pick touched never gets back in here to have its choice
    // overwritten. This line only runs for a signature nobody has overridden.
    currentSignature.current = signatureBlockStrings(nextSig);
    setBody(next.text);
    setHtmlBody(next.html);
    setEditorGeneration((g) => g + 1);
    // `body`/`htmlBody` are READ to decide whether the body is untouched and must
    // not re-run this effect: it fires on a From change, and depending on the body
    // would make every keystroke re-ask a question about the previous identity.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [identityId]);

  /** The From HINT: a NEW message to a contact who once reached the user at a
   * known alias defaults From to that alias (a concrete identity if one matches,
   * else the catch-all for its domain with the alias filled in). It never fights a
   * reply's own default, and it yields as soon as the user edits From themselves. */
  const applyAliasHint = (picked: ContactAddress): void => {
    if (parent || resumed || fromOverridden) return;
    const alias = (picked.reachedMeAt ?? "").trim();
    if (alias === "") return;
    const lower = alias.toLowerCase();
    const concrete = identities.find(
      (i) => i.enabled && !i.isWildcard && i.email.toLowerCase() === lower,
    );
    if (concrete) {
      setIdentityId(concrete.id);
      setAliasHint(alias);
      return;
    }
    const at = lower.lastIndexOf("@");
    const domain = at >= 0 ? lower.slice(at + 1) : "";
    const wild = identities.find(
      (i) => i.enabled && i.isWildcard && wildcardDomain(i).toLowerCase() === domain,
    );
    if (wild) {
      setIdentityId(wild.id);
      setLocalPart(alias);
      setAliasHint(alias);
    }
  };

  const pickIdentity = (id: string): void => {
    markTouched(); // a click in the From list: no input event (#614)
    setIdentityId(id);
    setFromOverridden(true);
    setAliasHint(null);
    const next = state.identities[id];
    // A concrete identity settles the address; a catch-all keeps whatever local
    // part is being typed at that domain.
    if (next && next.isWildcard) {
      const domain = wildcardDomain(next);
      setLocalPart((cur) => (cur.endsWith(`@${domain}`) ? cur : `@${domain}`));
    }
  };

  const note = aliasNote(identity, primary);

  /**
   * Who is about to see this alias for the first time (#533, spec §5).
   *
   * Distinct from `note` above, which says where replies will come BACK to. This
   * one is about disclosure, and it stays quiet on a plain reply — see
   * ./compose/aliasDisclosure.ts for why that is the whole design.
   *
   * The parent's To/Cc are passed WHEN PRESENT and not otherwise: a reply opens on
   * whatever the list loaded, which carries `from` alone. The module's comment
   * spells out which way that errs.
   */
  const disclosure = useMemo(
    () =>
      aliasDisclosure({
        alias: fromAddress,
        isAlias: note !== null,
        recipients: [...to, ...cc, ...bcc].map((c) => c.address),
        parentParticipants: parent
          ? [...(parent.from ?? []), ...(parent.to ?? []), ...(parent.cc ?? [])].map((a) => a.email)
          : null,
        ownDomains: identities.map((i) =>
          i.isWildcard ? wildcardDomain(i) : i.email.slice(i.email.lastIndexOf("@") + 1),
        ),
      }),
    [fromAddress, note, to, cc, bcc, parent, identities],
  );

  // -- attachments ------------------------------------------------------------

  /** Uploads each chosen file straight away and keeps its blobId. */
  const attach = async (files: FileList | null): Promise<void> => {
    if (!files || files.length === 0) return;
    // Marked here as well as by the DOM net: a file can arrive by a path that is
    // not an event inside the panel (#614).
    markTouched();
    setUploading(true);
    for (const file of Array.from(files)) {
      try {
        const res = await client.upload(file);
        setAttached((cur) => [
          ...cur,
          { blobId: res.blobId, name: file.name, type: file.type || res.type, size: res.size },
        ]);
      } catch (err) {
        pushToast({
          kind: "error",
          message: t("compose.attachFailed", {
            name: file.name,
            error: err instanceof Error ? err.message : String(err),
          }),
        });
      }
    }
    setUploading(false);
  };

  /** Uploads an image for inline embedding and registers it (kept out of the
   * visible tile list). Returns the cid to reference it by, or null on a failed
   * upload — the editor then inserts nothing. */
  const insertImageUpload = async (file: File): Promise<string | null> => {
    markTouched(); // #614
    try {
      const res = await client.upload(file);
      const cid = newCid();
      setInlineImages((cur) => [
        ...cur,
        {
          blobId: res.blobId,
          cid,
          name: file.name || "image",
          type: file.type || res.type || "application/octet-stream",
        },
      ]);
      return cid;
    } catch (err) {
      pushToast({
        kind: "error",
        message: t("compose.attachFailed", {
          name: file.name,
          error: err instanceof Error ? err.message : String(err),
        }),
      });
      return null;
    }
  };

  /**
   * The tiles: this composer's uploads, plus — when forwarding — the parent's own
   * attachments carried BY REFERENCE.
   *
   * A held part is a tile that says so and is excluded from the send
   * (AttachmentTiles.sendableAttachments): `resolveAttachments` refuses a
   * quarantined blobId by refusing the whole draft create, so leaving one in would
   * fail the send with a server error about a file, after the message was written.
   */
  const tiles: Tile[] = useMemo(() => {
    const uploads: Tile[] = attached.map((a) => ({
      kind: "upload",
      blobId: a.blobId,
      name: a.name,
      type: a.type,
      size: a.size,
    }));
    if (!forwarding) return uploads;
    const scan = forwarding.attachmentScan;
    const carried: Tile[] = (forwarding.attachments ?? [])
      .filter((p) => p.blobId)
      .map((p) => {
        const held = isWithheld(scan, p.partId) && !(p.partId !== null && released.has(p.partId));
        return {
          kind: "forward" as const,
          blobId: p.blobId,
          partId: p.partId,
          name: p.name ?? p.type,
          type: p.type,
          size: p.size,
          held,
          reasonKey: withheldReasonKey(scanStateFor(scan, p.partId)?.state),
        };
      });
    return [...carried, ...uploads];
  }, [attached, forwarding, released]);

  // -- autosave ---------------------------------------------------------------

  const [draftId, setDraftId] = useState<Id | null>(resumed?.id ?? null);
  const [saveState, setSaveState] = useState<SaveState>(
    resumed ? { kind: "saved" } : { kind: "never" },
  );

  const textOut = richMode ? htmlToText(htmlBody) : body;
  const markdownHtml = !richMode && markdown ? markdownToHtml(body) : "";
  const useHtml = richMode ? htmlBody.trim() !== "" : markdownHtml !== "";
  const htmlOut = useHtml ? (richMode ? htmlBody : markdownHtml) : "";

  const snapshot: DraftSnapshot = useMemo(
    () => ({
      from: fromAddress,
      to: to.map((c) => c.address),
      cc: cc.map((c) => c.address),
      bcc: bcc.map((c) => c.address),
      subject,
      text: textOut,
      html: htmlOut,
      // WHAT WOULD BE SAVED, not what is on screen: the ids `outgoingAttachments`
      // would actually send. So RELEASING a held forwarded part — which changes
      // nothing else about the composer — marks the draft dirty and gets the file
      // into the next save, and an inline image counts too.
      attachmentIds: outgoingAttachments(tiles, inlineImages, htmlOut).map((a) => a.blobId),
    }),
    [fromAddress, to, cc, bcc, subject, textOut, htmlOut, tiles, inlineImages],
  );

  /** The snapshot last SAVED, and when a save was last attempted. Refs rather than
   * state: nothing renders differently for either, and re-rendering on every tick
   * of a fifteen-second clock would be a render per three seconds for a panel
   * nobody is touching. */
  const savedSnapshot = useRef<DraftSnapshot | null>(resumed ? snapshot : null);
  const lastAttemptAt = useRef<number | null>(null);
  const savingRef = useRef(false);
  /** The live snapshot, for the interval and the blur handler — both run outside
   * the render that produced them. */
  const snapshotRef = useRef(snapshot);
  snapshotRef.current = snapshot;
  const draftIdRef = useRef(draftId);
  draftIdRef.current = draftId;
  const parentRefs = useMemo(
    () =>
      parent
        ? {
            inReplyTo: parent.messageId ?? [],
            references: [...(parent.references ?? []), ...(parent.messageId ?? [])].filter(Boolean),
          }
        : { inReplyTo: [], references: [] },
    [parent],
  );
  const parentRefsRef = useRef(parentRefs);
  parentRefsRef.current = parentRefs;
  const tilesRef = useRef(tiles);
  tilesRef.current = tiles;
  const identityRef = useRef(identity);
  identityRef.current = identity;
  const inlineRef = useRef(inlineImages);
  inlineRef.current = inlineImages;
  /** The chips themselves, so a save keeps the display names the snapshot does not
   * carry (the snapshot compares ADDRESSES — a name only ever arrives with the
   * address it came from, so it cannot change on its own). */
  const recipientsRef = useRef({ to, cc, bcc });
  recipientsRef.current = { to, cc, bcc };
  /** A ref, NOT a dependency of runAutosave: the unmount flush below is keyed on
   * runAutosave's identity, so a callback that changed identity would fire a
   * "last save on the way out" on every render that produced a new one. */
  const onDraftSavedRef = useRef(onDraftSaved);
  onDraftSavedRef.current = onDraftSaved;

  const runAutosave = useCallback(
    async (onBlur: boolean): Promise<void> => {
      const snap = snapshotRef.current;
      const decision = autosaveDecision({
        dirty: draftDirty(savedSnapshot.current, snap),
        empty: draftEmpty(snap),
        touched: touchedRef.current,
        saving: savingRef.current,
        lastAttemptAt: lastAttemptAt.current,
        now: Date.now(),
        intervalMs: AUTOSAVE_INTERVAL_MS,
        onBlur,
      });
      if (!decision.save) return;
      savingRef.current = true;
      lastAttemptAt.current = Date.now();
      setSaveState({ kind: "saving" });
      const res = await actions.saveDraft({
        previousDraftId: draftIdRef.current,
        from: { name: identityRef.current?.name || null, email: snap.from },
        to: recipientsRef.current.to.map(asAddress),
        ...(recipientsRef.current.cc.length
          ? { cc: recipientsRef.current.cc.map(asAddress) }
          : {}),
        ...(recipientsRef.current.bcc.length
          ? { bcc: recipientsRef.current.bcc.map(asAddress) }
          : {}),
        subject: snap.subject,
        text: snap.text,
        ...(snap.html ? { html: snap.html } : {}),
        ...(parentRefsRef.current.inReplyTo.length
          ? { inReplyTo: parentRefsRef.current.inReplyTo }
          : {}),
        ...(parentRefsRef.current.references.length
          ? { references: parentRefsRef.current.references }
          : {}),
        ...(() => {
          const list = outgoingAttachments(tilesRef.current, inlineRef.current, snap.html);
          return list.length ? { attachments: list } : {};
        })(),
      });
      savingRef.current = false;
      // A REFUSED CREATE CAN STILL HAVE RETIRED THE PREVIOUS VERSION (the two
      // Email/set loops are independent — core/actions.ts saveDraft says so), so
      // the id is forgotten whenever the server says it retired it. Aiming the
      // next save's destroy at a draft that no longer exists would make every
      // later save carry a `notDestroyed` alongside it.
      // The REF is written alongside the state, not left to the next render: the
      // next save reads it to know what to destroy, and a stale read there would
      // orphan the draft this one just created.
      if (res.retiredPrevious) {
        draftIdRef.current = null;
        setDraftId(null);
      }
      if (res.ok) {
        savedSnapshot.current = snap;
        draftIdRef.current = res.draftId;
        setDraftId(res.draftId);
        setSaveState({ kind: "saved" });
        onDraftSavedRef.current?.(res.draftId);
        return;
      }
      setSaveState({ kind: "failed", reason: res.reason });
    },
    [actions],
  );

  /**
   * The composer is FINISHED — sent, or discarded — so nothing may write a draft
   * for it again.
   *
   * ⚠ WITHOUT THIS, THE FLUSH BELOW RESURRECTS WHAT WAS JUST DESTROYED. Both Send
   * and Discard call `onClose`, which changes the route and unmounts this
   * component; the unmount flush would then see a dirty snapshot and create a
   * fresh draft — after the message had gone, or after the user asked for it to be
   * thrown away. A ref rather than state because it is read in a cleanup that runs
   * after the last render.
   */
  const finished = useRef(false);

  useEffect(() => {
    const timer = setInterval(() => void runAutosave(false), AUTOSAVE_TICK_MS);
    return () => clearInterval(timer);
  }, [runAutosave]);

  /**
   * ONE LAST SAVE ON THE WAY OUT.
   *
   * Closing the composer is not an instruction to throw anything away — Discard is
   * — so up to fifteen seconds of typing must not go with it. The save is fired
   * from the unmount cleanup and deliberately not awaited: there is nothing left to
   * render the result into, and the request completes regardless. Every guard still
   * applies (`autosaveDecision`), so a clean or empty composer writes nothing, and
   * `finished` stops it entirely once the message has gone.
   */
  useEffect(
    () => () => {
      if (finished.current) return;
      void runAutosave(true);
    },
    [runAutosave],
  );

  // -- escape, focus ----------------------------------------------------------

  // ESCAPE MUST CLOSE THE SUB-DIALOG, NOT THE COMPOSE WINDOW.
  //
  // The shell's Escape chain hands Escape to escapeNow, whose first layer is "drop
  // ?compose= from the route". Without this, an Escape aimed at the send-later or
  // template dialog closes the whole composer. A CAPTURE-phase window listener
  // sees the event before the shell's bubble-phase one and stops it there — the
  // same technique the attachment lightbox uses for the same reason.
  const subDialogOpen = snippetPicker !== null || sendLaterOpen;
  useEffect(() => {
    if (!subDialogOpen) return;
    const onKey = (ev: KeyboardEvent): void => {
      if (ev.key !== "Escape") return;
      ev.stopPropagation();
      setSnippetPicker(null);
      setSendLaterOpen(false);
    };
    window.addEventListener("keydown", onKey, true);
    return () => window.removeEventListener("keydown", onKey, true);
  }, [subDialogOpen]);

  /**
   * FOCUS GOES IN, AND COMES BACK OUT WHERE IT WAS (#447).
   *
   * The element that had focus when the composer opened is the row the user
   * pressed Reply on, so it is where focus returns on close — otherwise a keyboard
   * user lands at the top of the document and has to walk back down to the message
   * they were reading.
   */
  const returnFocusTo = useRef<HTMLElement | null>(null);
  useEffect(() => {
    returnFocusTo.current = document.activeElement as HTMLElement | null;
    const root = panelRef.current;
    // A composer that already has recipients — a reply, a forward, a resumed draft
    // — opens with the caret in the BODY, which is the only field the user is going
    // to touch. A blank one opens in To.
    const target =
      intent.kind === "new"
        ? root?.querySelector<HTMLElement>(".compose-chip-text")
        : root?.querySelector<HTMLElement>(".editor-surface, .textarea");
    (target ?? root?.querySelector<HTMLElement>("input, textarea"))?.focus();
    return () => {
      const el = returnFocusTo.current;
      // Only if it is still in the document: a row that has since been archived is
      // gone, and focusing a detached node silently focuses <body>.
      if (el && el.isConnected) {
        el.focus();
        return;
      }
      // ...and when it is gone, the LIST rather than `<body>` (#447). Focus on the
      // body is where the desk's grid stops answering arrows and a screen reader
      // loses its place entirely; the grid remembers which row it was on.
      focusMessageListSoon();
    };
  }, []);

  /**
   * THE FOCUS TRAP, and only where it is honest (./compose/placement.ts
   * trapsFocus): while the composer has the main area or the whole phone screen,
   * Tab must not walk out of it into a list nobody can see. In the PANE placement
   * the list is visibly beside the composer and Tab reaches it, which is the
   * behaviour the layout promises.
   */
  const trap = trapsFocus(placement);
  useEffect(() => {
    if (!trap) return;
    // The selector, the visibility filter and the wrap decision are ui/focusTrap.ts
    // now (#447) — this component had the most careful of the three copies, which
    // is why that module inherited its selector and its `offsetParent` test.
    const onKey = (ev: KeyboardEvent): void => trapTab(panelRef.current, ev);
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [trap]);

  // Toggle rich/plain, carrying the content across so nothing typed is lost.
  const toggleRich = (): void => {
    markTouched(); // a button: rewrites the body with no input event (#614)
    if (richMode) {
      setBody(htmlToText(htmlBody));
      setRichMode(false);
    } else {
      setHtmlBody(textToHtml(body));
      setRichMode(true);
      setEditorGeneration((g) => g + 1);
    }
  };

  /**
   * A signature picked BY HAND (#649) — any saved signature, not just the one
   * automatically seeded for the From identity. `pickSignatureFor`
   * (./compose/signature.ts) decides replace vs insert for whichever body is
   * LIVE (richMode picks which); `currentSignature` is updated to the newly
   * picked block either way, so a second manual pick compares against what
   * THIS one left rather than the original default.
   *
   * Mirrors the template picker below: `setEditorGeneration` remounts the
   * uncontrolled rich editor, because this writes `htmlBody` from OUTSIDE the
   * editor's own `onChange` just as a template insert does.
   */
  const applyPickedSignature = (picked: ComposeSnippet): void => {
    const next = signatureBlockStrings({ html: picked.bodyHtml, text: picked.bodyText }) ?? {
      text: "",
      html: "",
    };
    if (richMode) {
      const result = pickSignatureFor(htmlBody, currentSignature.current?.html ?? null, next.html);
      setHtmlBody(result.body);
      setEditorGeneration((g) => g + 1);
    } else {
      const result = pickSignatureFor(body, currentSignature.current?.text ?? null, next.text);
      setBody(result.body);
    }
    currentSignature.current = next;
  };

  // -- send -------------------------------------------------------------------

  const discard = async (): Promise<void> => {
    const id = draftIdRef.current;
    // Marked BEFORE the close, or the unmount flush would re-create the draft this
    // is about to destroy.
    finished.current = true;
    onClose();
    if (id) await actions.discardDraft(id);
  };

  const send = async (sendAt?: string, opts: { archive?: boolean } = {}): Promise<void> => {
    if (allChips.length === 0) {
      pushToast({ kind: "error", message: t("compose.needRecipient") });
      return;
    }
    if (fromAddress === "" || fromAddress.startsWith("@")) {
      pushToast({ kind: "error", message: t("compose.needFrom") });
      return;
    }
    setSending(true);
    const exact = identity && !identity.isWildcard && identity.email === fromAddress;

    const attachments = outgoingAttachments(tiles, inlineImages, htmlOut);

    // SEND & ARCHIVE (#648). Only ever offered for reply/replyAll/forward
    // (offersSendAndArchive), which is exactly when `parent` is set, so this is
    // the one message to archive. The SERVER archives it, and only once the
    // message has actually been sent (./compose/sendArchive.ts explains why the
    // client no longer does it itself).
    const archiveOnSuccess = archiveOnSuccessFor(opts.archive === true, parent?.id ?? null);

    const res = await actions.sendMessage({
      from: { name: identity?.name || null, email: fromAddress },
      to: to.map(asAddress),
      ...(cc.length ? { cc: cc.map(asAddress) } : {}),
      ...(bcc.length ? { bcc: bcc.map(asAddress) } : {}),
      subject,
      text: textOut,
      ...(htmlOut ? { html: htmlOut } : {}),
      ...(parentRefs.inReplyTo.length ? { inReplyTo: parentRefs.inReplyTo } : {}),
      ...(parentRefs.references.length ? { references: parentRefs.references } : {}),
      ...(exact && identity ? { identityId: identity.id } : {}),
      ...(attachments.length ? { attachments } : {}),
      ...(sendAt ? { sendAt } : {}),
      ...(archiveOnSuccess ? { archiveOnSuccess } : {}),
      ...(encrypting ? { encrypt: true } : {}),
    });
    setSending(false);
    if (!res) return; // the error toast already carries the server's own words

    /**
     * SEND & ARCHIVE CLOSES THE CONVERSATION LIKE ARCHIVE DOES (#648). Decided
     * here, where it is known what was sent and whether the pane underneath is
     * its conversation; carried out by the list surface once this composer has
     * closed (ui/useListVerbs.ts), through the same `advancePastOpen` as `e`.
     * Immediately, not when the server archives — see ./compose/sendArchive.ts.
     * Written BEFORE `onClose`, whose navigation is what the surface reacts to.
     */
    const leavePaneIfArchiving = (archiving: boolean): void => {
      const openId = currentRoute().m ?? null;
      const leave = paneToLeaveAfterSend({
        archiving,
        parent: parent ? { id: parent.id, threadId: parent.threadId ?? null } : null,
        open: { id: openId, threadId: openId ? (state.emails[openId]?.threadId ?? null) : null },
      });
      if (leave) requestPaneLeave(leave);
    };

    if (res.refused) {
      // REFUSED (#692): the server said no (noRecipients, forbiddenFrom, ...) and
      // `sendMessage` already toasted its words. Nothing was sent, so nothing is
      // announced, the autosave stays (it is the only saved copy of what was
      // written), no pane is left and the composer stays open for a retry. The
      // draft this send created is a redundant copy of the autosave and the next
      // send makes its own, so retire it quietly rather than pile up Drafts rows.
      if (res.emailId) void actions.discardDraft(res.emailId, { quiet: true });
      return;
    }

    if (res.queued) {
      // HELD, not sent (#62). No undo action offered: there is no submission to
      // cancel yet, and offering one would be a button that cannot do what it says.
      // NOT `finished`: the message has not gone and no draft has been created
      // server-side (queueSend inserts no optimistic row), so the flush on the way
      // out is the only thing that can save what was written. It will fail too —
      // the server is unreachable — and the footer said so.
      //
      // The archive request is held WITH the send (core/sendQueue.ts), so it too
      // happens only once the message has actually gone.
      pushToast({ kind: "info", message: t(queuedToastKey(archiveOnSuccess !== undefined)) });
      leavePaneIfArchiving(archiveOnSuccess !== undefined);
      onClose();
      return;
    }

    // THE AUTOSAVED DRAFT IS NOT THE MESSAGE THAT WENT. `sendMessage` creates its
    // own draft and submits THAT (core/actions.ts), so the version autosave has
    // been keeping is now a superseded copy sitting in Drafts. Retired after the
    // send rather than in the same call, deliberately: if the send fails there
    // must still be a saved draft, and the worst case here is one visible,
    // deletable draft row — the same trade core/sendQueue.ts takes for the same
    // reason. Quiet, because a cleanup failure is not something the user asked for.
    const superseded = draftIdRef.current;
    finished.current = true;
    if (superseded) void actions.discardDraft(superseded, { quiet: true });

    const submissionId = res.submissionId;
    // THE HOLD WINDOW, AS THE SERVER SET IT (#375 increment 6). An immediate send
    // comes back with a `sendAt` of "now + the server's Undo-Send window", which is
    // exactly the deadline of the offer — so the toast counts it down instead of
    // the client guessing at a setting it does not own.
    const holdUntil = !sendAt && res.sendAt ? Date.parse(res.sendAt) : Number.NaN;

    /**
     * UNDO SEND. Nothing else to reverse, even after Send & archive (#648): the
     * server archives only once the message has actually been sent, and a
     * cancelled submission never is — so there is no archive to put back.
     */
    const unsend = async (): Promise<boolean> => {
      if (!submissionId) return false;
      return actions.cancelSubmission(submissionId);
    };

    const sentMessage = t(sentToastKey(res.archivePending === true));

    if (submissionId && Number.isFinite(holdUntil) && holdUntil > Date.now()) {
      // Joins the undo LOG as well as the toast, so Ctrl/⌘+Z reaches an unsend
      // exactly as it reaches an archive.
      offerUndo(sentMessage, unsend, holdUntil - Date.now());
    } else {
      pushToast({
        kind: "info",
        // A SCHEDULED send says WHEN, and keeps the plain button rather than a
        // countdown: its window is however long until Tuesday, so a ticking "3 d"
        // would be a deadline dressed up as an urgent one. (Send & archive is not
        // offered alongside Send Later, so `sentMessage` here is never the
        // will-archive variant in practice — it is still the server's echo, not
        // an assumption.)
        message: sendAt ? t("sendLater.scheduled", { when: formatWake(sendAt) }) : sentMessage,
        ...(submissionId
          ? { action: { label: t("undo.unsend"), run: () => void unsend() } }
          : {}),
      });
    }
    // The server's echo decides, as it does for the toast: a server that
    // ignored the archive request will not archive, so the pane stays.
    leavePaneIfArchiving(res.archivePending === true);
    onClose();
  };

  const title =
    intent.kind === "reply"
      ? t("compose.replyTitle")
      : intent.kind === "replyAll"
        ? t("compose.replyAllTitle")
        : intent.kind === "forward"
          ? t("compose.forwardTitle")
          : intent.kind === "draft"
            ? t("compose.draftTitle")
            : t("compose.title");

  return (
    <section
      className={`compose compose-${placement}`}
      role="dialog"
      aria-label={title}
      // A trapped composer IS modal; one sharing the screen with a live list is
      // not, and claiming otherwise would tell a screen reader the list beside it
      // does not exist.
      aria-modal={trap ? true : undefined}
      ref={panelRef}
    >
      <header className="compose-header">
        <h2>{title}</h2>
        <button
          type="button"
          className="icon-button"
          aria-label={t("action.close")}
          onClick={onClose}
        >
          ✕
        </button>
      </header>

      <form
        className="compose-form"
        onSubmit={(ev) => {
          ev.preventDefault();
          void send();
        }}
        onKeyDown={(ev) => {
          // Ctrl/⌘+Enter sends from anywhere in the composer (#149) — on the FORM
          // rather than on each field, so it works in the body, where plain Enter
          // is a newline and must stay one. +Shift is Send & archive (#648): free
          // everywhere this combo runs (Shell's own shortcuts bail out on ANY
          // Ctrl/Cmd-held keydown — ui/Shell.tsx), and gated by the SAME pure
          // check the button beside Send uses (./compose/sendArchive.ts
          // offersSendAndArchive), so the shortcut can never do something the
          // button is not also there to offer.
          if (ev.key !== "Enter" || !(ev.ctrlKey || ev.metaKey)) return;
          ev.preventDefault();
          if (sending || uploading) return;
          if (ev.shiftKey) {
            if (canSendAndArchive) void send(undefined, { archive: true });
            return;
          }
          void send();
        }}
      >
        <FromLine
          identity={identity}
          identities={identities}
          parent={parentFacts}
          localPart={localPart}
          onLocalPart={(v) => {
            setLocalPart(v);
            setFromOverridden(true);
            setAliasHint(null);
          }}
          onPick={pickIdentity}
        />
        {aliasHint ? (
          <p className="hint compose-alias-hint">{t("compose.aliasHint", { alias: aliasHint })}</p>
        ) : null}

        <RecipientChips
          label={t("compose.to")}
          chips={to}
          onChips={(c) => {
            markTouched();
            setTo(c);
          }}
          factFor={chipFactFor}
          onNeedFact={needFact}
          onPick={applyAliasHint}
          keysAvailable={keysAvailable}
          autoFocus={to.length === 0}
        />

        {ccOpen ? (
          <RecipientChips
            label={t("compose.cc")}
            chips={cc}
            onChips={(c) => {
              markTouched();
              setCc(c);
            }}
            factFor={chipFactFor}
            onNeedFact={needFact}
            keysAvailable={keysAvailable}
          />
        ) : null}
        {bccOpen ? (
          <RecipientChips
            label={t("compose.bcc")}
            chips={bcc}
            onChips={(c) => {
              markTouched();
              setBcc(c);
            }}
            factFor={chipFactFor}
            onNeedFact={needFact}
            keysAvailable={keysAvailable}
          />
        ) : null}
        {!ccOpen || !bccOpen ? (
          <p className="compose-more-recipients">
            {!ccOpen ? (
              <button type="button" className="link-button" onClick={() => setCcOpen(true)}>
                {t("compose.addCc")}
              </button>
            ) : null}
            {!bccOpen ? (
              <button type="button" className="link-button" onClick={() => setBccOpen(true)}>
                {t("compose.addBcc")}
              </button>
            ) : null}
          </p>
        ) : null}

        <label className="field">
          <span className="field-label">{t("compose.subject")}</span>
          <input
            className="text-input"
            value={subject}
            onChange={(ev) => setSubject(ev.target.value)}
          />
        </label>

        <div className="field field-grow">
          <div className="editor-head">
            <span className="field-label">{t("compose.body")}</span>
            <button type="button" className="link-button editor-mode-toggle" onClick={toggleRich}>
              {richMode ? t("compose.plainMode") : t("compose.richMode")}
            </button>
            {/* MARKDOWN is an option on PLAIN mode, not a third editor (#56). What
                you type stays the text part either way, and ticking this adds a
                rendered HTML part alongside it — nothing to convert, nothing to
                lose, so the toggle can never destroy what you wrote. */}
            {!richMode ? (
              <label className="editor-markdown-toggle">
                <input
                  type="checkbox"
                  checked={markdown}
                  onChange={(ev) => setMarkdown(ev.target.checked)}
                />
                <span>{t("compose.markdownMode")}</span>
              </label>
            ) : null}
          </div>
          {richMode ? (
            <RichEditor
              key={`rich-${editorGeneration}`}
              initialHTML={htmlBody}
              onChange={(html) => {
                markTouched();
                setHtmlBody(html);
              }}
              onImageFile={insertImageUpload}
              onBlur={() => void runAutosave(true)}
              label={t("compose.body")}
            />
          ) : (
            <textarea
              className="textarea"
              value={body}
              rows={10}
              aria-label={t("compose.body")}
              onChange={(ev) => setBody(ev.target.value)}
              onBlur={() => void runAutosave(true)}
            />
          )}
        </div>

        {/* The alias note: what a reply to this message will come back to. Muted,
            under the body, and absent on the primary address — where it would say
            nothing. */}
        {note ? <p className="hint compose-alias-note">{t("compose.aliasNote", note)}</p> : null}
        {/* Disclosure (#533): rare by construction, so it is a line rather than a
            dialog, and it names the people rather than the rule. */}
        {disclosure ? (
          <p className="hint compose-alias-disclosure">
            {disclosure.newRecipients.length === 1
              ? t("compose.aliasDisclosure.one", {
                  alias: disclosure.alias,
                  who: disclosure.newRecipients[0] as string,
                })
              : t("compose.aliasDisclosure.other", {
                  alias: disclosure.alias,
                  n: disclosure.newRecipients.length,
                  who: disclosure.newRecipients.join(", "),
                })}
          </p>
        ) : null}

        <AttachmentTiles
          tiles={tiles}
          uploading={uploading}
          onRemove={(blobId) => {
            markTouched();
            setAttached((cur) => cur.filter((a) => a.blobId !== blobId));
          }}
          onFiles={(files) => void attach(files)}
          // `actions.releaseAttachment` has already re-read the thread; what it
          // cannot do is change the FROZEN parent this composer holds, so the tile
          // stops being held from this local record instead.
          onReleased={(partId) => {
            markTouched();
            setReleased((cur) => new Set(cur).add(partId));
          }}
          parent={
            forwarding?.threadId
              ? { threadId: forwarding.threadId, emailId: forwarding.id }
              : null
          }
        />

        {/* `ownKey`/`ownKeyReason` are synchronous (state.identities + the
            already-fetched session), so — unlike the old `UserKey/get`-based
            reading — there is no "answer in flight" state to render around
            here. */}
        <SendStatusLine
          ownKey={ownKey}
          ownAddress={fromAddress}
          chips={sendLineChips}
          reason={ownKeyReason}
        />
        <EncryptSwitch
          offer={offer}
          checked={encrypting}
          onChange={setWantEncrypt}
          names={[...to, ...cc].map((c) => chipLabel(c))}
          keys={recipientKeys}
          onDecideKey={decideRecipientKey}
          onImportKey={setImportKeyFor}
          onRefreshKey={refreshRecipientKey}
          recipients={lookupAddresses}
          labels={Object.fromEntries([...to, ...cc].map((c) => [c.address.trim().toLowerCase(), chipLabel(c)]))}
          warnDays={client.pgpKeyExpiryWarningDays()}
        />

        <div className="compose-actions">
          <button
            type="submit"
            className="button button-primary"
            disabled={sending || uploading}
            title={t("compose.sendShortcut")}
          >
            {sending ? t("compose.sending") : t("action.send")}
          </button>
          {/* SEND & ARCHIVE (#648): beside Send, only for a reply/reply-all/forward
              (there is a conversation to close out) — never a new message or a
              resumed draft, which have none. Sends exactly like Send, then
              archives the same message `e` would (see the `send` function's own
              comment for why that is "the conversation" here). */}
          {canSendAndArchive ? (
            <button
              type="button"
              className="button"
              disabled={sending || uploading}
              title={t("compose.sendArchiveShortcut")}
              onClick={() => void send(undefined, { archive: true })}
            >
              {t("action.sendAndArchive")}
            </button>
          ) : null}
          {/* SEND LATER (#56). Beside Send rather than inside it: scheduling is a
              different act from sending, and burying it in a split button makes the
              common case worse to serve the rare one. */}
          <button
            type="button"
            className="button"
            disabled={sending || uploading}
            aria-haspopup="dialog"
            onClick={() => setSendLaterOpen(true)}
          >
            {t("sendLater.open")}
          </button>
          {/* A DEFAULT SIGNATURE is already in the body when this opens
              (Compose.tsx's `openingBodies`) — these two controls are for
              everything else: any OTHER saved signature (#649, picked by hand,
              only when at least one exists) and a template (only before you
              start writing, SnippetPicker's own rule). */}
          {hasSignatures ? (
            <button
              type="button"
              className="button button-quiet"
              disabled={sending}
              aria-haspopup="dialog"
              onClick={() => setSnippetPicker("signature")}
            >
              {t("compose.insertSignature")}
            </button>
          ) : null}
          {hasTemplates ? (
            <button
              type="button"
              className="button button-quiet"
              disabled={sending}
              aria-haspopup="dialog"
              onClick={() => setSnippetPicker("template")}
            >
              {t("compose.insertTemplate")}
            </button>
          ) : null}
          {/* data-state is the same fact as the words, for a reader that cannot
              read them: scripts/a11y-check.cjs waits on it before Discard, since a
              Discard pressed mid-save cannot destroy the draft that save creates. */}
          <span className="compose-save-state" data-state={saveState.kind}>
            {saveState.kind === "saving"
              ? t("compose.saving")
              : saveState.kind === "saved"
                ? t("compose.saved")
                : saveState.kind === "failed"
                  ? t("compose.notSaved", { reason: saveState.reason })
                  : t("compose.notSavedYet")}
          </span>
          <button
            type="button"
            className="button button-quiet compose-discard"
            disabled={sending}
            onClick={() => void discard()}
          >
            {t("compose.discard")}
          </button>
        </div>
      </form>

      {snippetPicker ? (
        <div className="modal-backdrop" onKeyDown={(e) => e.key === "Escape" && setSnippetPicker(null)}>
          <SnippetPicker
            kind={snippetPicker}
            snippets={snippets}
            identityId={identity?.id ?? null}
            bodyUntouched={untouched && intent.kind !== "draft"}
            onClose={() => setSnippetPicker(null)}
            onPick={(s) => {
              markTouched(); // a pick from a list: no input event (#614)
              setSnippetPicker(null);
              if (snippetPicker === "signature") {
                // #649: replace-or-insert, decided in ./compose/signature.ts —
                // see applyPickedSignature's own comment.
                applyPickedSignature(s);
                return;
              }
              // A TEMPLATE goes in at the top, above any signature and quote
              // already there, and never deletes what is present — compose has no
              // undo, so nothing here may destroy text. It writes to whichever body
              // the editor is showing, or the insert would land in the mode the
              // user is not looking at.
              if (richMode) {
                const add = s.bodyHtml || textToHtml(s.bodyText);
                setHtmlBody((cur) => `${add}${cur}`);
                setEditorGeneration((g) => g + 1);
              } else {
                const add = s.bodyText || htmlToText(s.bodyHtml);
                setBody((cur) => `${add}${cur}`);
              }
            }}
          />
        </div>
      ) : null}

      {importKeyFor !== null ? (
        <ImportKeyDialog
          address={importKeyFor}
          check={(key) => actions.importContactKey(importKeyFor, key, { dryRun: true })}
          onImport={(key) => actions.importContactKey(importKeyFor, key)}
          onImported={() => void refreshRecipientKeys([importKeyFor])}
          onClose={() => setImportKeyFor(null)}
        />
      ) : null}

      {sendLaterOpen ? (
        <div className="modal-backdrop" onKeyDown={(e) => e.key === "Escape" && setSendLaterOpen(false)}>
          <SendLaterMenu
            maxDelayedSendSeconds={maxDelayedSendSeconds}
            onClose={() => setSendLaterOpen(false)}
            onPick={(iso) => {
              setSendLaterOpen(false);
              void send(iso);
            }}
          />
        </div>
      ) : null}
    </section>
  );
}
