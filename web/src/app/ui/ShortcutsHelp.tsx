// SPDX-License-Identifier: AGPL-3.0-only

// The "?" shortcut sheet. Every shortcut listed here is implemented — the chain
// in ui/Shell.tsx, the per-surface verbs in DeskRoute/TrayRoute/ListRoute, and
// the `g` table in ui/goto.ts — because a help screen that lies is worse than
// none.
//
// THE DESK KEYS ARE HERE TOO (#375). `1`–`9` and Tab landed with the desk and
// `g g` / `g 0`–`g 9` with the account switcher; none of them is printed on the
// surface that has it (ui/desk/SelectionToolbar.tsx says why), so this sheet is
// the ONLY place they are written down. A shortcut nobody can discover is a
// shortcut that does not exist.
//
// `f` IS NOT ONE OF THOSE ANY MORE. It landed with the desk as the tray's own
// key; #582 gave it to Forward everywhere instead, and #624 §1 fixed the desk
// and the in-tray, where a leftover `case "f":` on the tray toggle shadowed
// Forward's own case and made it unreachable there. `s` is the tray/star
// toggle's key now, on every surface.

import { useEffect, useRef } from "react";
import type { JSX } from "react";

import { t } from "../i18n";

const KEYS: [string, Parameters<typeof t>[0]][] = [
  // The list is a `role="grid"` now (#447): while focus is in it the arrows, the
  // ends and the page keys are its own, and `j`/`k` keep working from anywhere on
  // the surface.
  ["j / k / ↑ / ↓", "shortcuts.navigate"],
  ["Home / End", "shortcuts.listEnds"],
  ["PageUp / PageDown", "shortcuts.listPage"],
  ["Enter / o", "shortcuts.open"],
  ["u / Esc", "shortcuts.back"],
  ["e", "shortcuts.archive"],
  ["#", "shortcuts.trash"],
  ["s", "shortcuts.star"],
  ["p", "shortcuts.pin"],
  ["b", "shortcuts.snooze"],
  ["!", "shortcuts.important"],
  ["m", "shortcuts.mute"],
  ["Shift+I / Shift+U", "shortcuts.read"],
  ["x / Space", "shortcuts.select"],
  ["Shift+click", "shortcuts.selectRange"],
  ["Ctrl+click / ⌘+click", "shortcuts.selectOne"],
  ["l", "action.label"],
  ["l then 1–9", "shortcuts.labelFast"],
  ["-", "queue.dismissOne"],
  ["r", "shortcuts.reply"],
  // Reply all (#646), everywhere `r` reply works. `a` is free on every surface —
  // it does not collide with `g a` (Archive), a two-key sequence Shell consumes
  // entirely before a bare `a` ever reaches a list's own handler (ui/goto.ts).
  ["a", "shortcuts.replyAll"],
  // Forward (#582), everywhere `r` reply works — #624 §1 fixed the desk and the
  // in-tray, where it used to share a case with the tray toggle and could never
  // fire (see DeskRoute.tsx/TrayRoute.tsx's own comments at that case).
  ["f", "shortcuts.forward"],
  ["c", "shortcuts.compose"],
  ["Ctrl/⌘+Enter", "shortcuts.sendCompose"],
  // #648. Free everywhere Ctrl/⌘+Enter runs (Shell's own shortcuts already bail
  // out on any Ctrl/Cmd-held keydown — ui/Shell.tsx), and only does anything
  // on a reply/reply-all/forward (./compose/sendArchive.ts offersSendAndArchive).
  ["Ctrl/⌘+Shift+Enter", "shortcuts.sendArchiveCompose"],
  ["/", "shortcuts.search"],
  ["Ctrl/⌘+K", "shortcuts.palette"],
  // #375 increment 6. Deliberately NOT printed on the toast's own button: the
  // sheet is the one place a shortcut is written down, and a key legend on a
  // control that is on screen for eight seconds is a legend nobody reads.
  ["Ctrl/⌘+Z", "shortcuts.undo"],
  // #447: the same offer, reached rather than run — for a reader who wants to hear
  // the sentence before deciding. Focusing it also stops the countdown.
  ["g then u", "shortcuts.undoToast"],
  ["1–9", "shortcuts.refile"],
  // #375 increment 7 / #464. The desk only: they are a statement about where a
  // message belongs on THIS partition, and there is no line in the message list
  // to cross.
  ["Shift+↑", "shortcuts.toSurface"],
  ["Shift+↓", "shortcuts.toDrawer"],
  // ⚠ NOT Tab any more (#447). Tab was swallowed while focus was inside the list,
  // which meant it could never reach the controls INSIDE the list — a section's
  // fold, a drawer's Open, the line's stricter/looser — and a keyboard user could
  // not leave the list at all. The halves are a real tablist now, so the arrows
  // move between them with the standard keys and Tab is Tab again.
  ["← / →", "shortcuts.deskTabs"],
  ["g then i/p/n/s/b/e/d/t/a", "shortcuts.goto"],
  ["g then g", "shortcuts.switcher"],
  ["g then 0–9", "shortcuts.scope"],
  ["?", "shortcuts.help"],
];

export function ShortcutsHelp({ onClose }: { onClose: () => void }): JSX.Element {
  const ref = useRef<HTMLButtonElement>(null);
  useEffect(() => ref.current?.focus(), []);

  return (
    <div className="sheet" role="dialog" aria-modal="true" aria-label={t("shortcuts.title")}>
      <header className="sheet-header">
        <h2>{t("shortcuts.title")}</h2>
        <button ref={ref} type="button" className="icon-button" aria-label={t("action.close")} onClick={onClose}>
          ✕
        </button>
      </header>
      <dl className="shortcut-list">
        {KEYS.map(([key, label]) => (
          <div className="shortcut" key={key}>
            <dt>
              <kbd>{key}</kbd>
            </dt>
            <dd>{t(label)}</dd>
          </div>
        ))}
      </dl>
    </div>
  );
}
