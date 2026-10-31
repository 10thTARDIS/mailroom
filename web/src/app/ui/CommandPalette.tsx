// SPDX-License-Identifier: AGPL-3.0-only

// The command palette (spec §12: "keyboard-driven power use: shortcuts + a
// command palette"). Ctrl/⌘-K. Solid panel, no scrim, arrow keys + Enter.
//
// Anything typed that matches no command is run as a SEARCH, so the palette
// doubles as a second search entry point over the same operator syntax.

import { useEffect, useMemo, useRef, useState } from "react";
import type { JSX } from "react";

import { useApp, useMailState } from "../state";
import { t } from "../i18n";
import type { View } from "../../core/views";

interface Props {
  onClose: () => void;
  onCompose: () => void;
  onShowShortcuts: () => void;
  onLabel: () => void;
  onArchive: () => void;
  onTrash: () => void;
  onMarkRead: () => void;
  onMarkUnread: () => void;
  onPin: () => void;
  onSnooze: () => void;
  onImportant: () => void;
  onMute: () => void;
  onSearch: (q: string) => void;
  hasSelection: boolean;
  /** Present only when a message is open — printing needs something to print. */
  onPrint?: (() => void) | undefined;
}

interface Command {
  id: string;
  label: string;
  run: () => void;
}

export function CommandPalette(props: Props): JSX.Element {
  const { setView } = useApp();
  const state = useMailState();
  const [query, setQuery] = useState("");
  const [index, setIndex] = useState(0);
  const inputRef = useRef<HTMLInputElement>(null);

  useEffect(() => inputRef.current?.focus(), []);

  const commands = useMemo<Command[]>(() => {
    const go = (v: View, label: string): Command => ({
      id: `go:${label}`,
      label: `Go to ${label}`,
      run: () => setView(v),
    });
    const list: Command[] = [
      { id: "compose", label: t("action.compose"), run: props.onCompose },
      go({ kind: "role", role: "inbox" }, t("nav.inbox")),
      go({ kind: "starred" }, t("nav.starred")),
      go({ kind: "priority" }, t("nav.priority")),
      go({ kind: "needsCategory" }, t("nav.needsCategory")),
      go({ kind: "snoozed" }, t("nav.snoozed")),
      go({ kind: "unread" }, t("nav.unread")),
      go({ kind: "role", role: "sent" }, t("nav.sent")),
      go({ kind: "role", role: "drafts" }, t("nav.drafts")),
      go({ kind: "role", role: "archive" }, t("nav.archive")),
      go({ kind: "role", role: "trash" }, t("nav.trash")),
      { id: "shortcuts", label: t("settings.shortcuts"), run: props.onShowShortcuts },
    ];
    if (props.onPrint) {
      list.push({ id: "print", label: t("action.print"), run: props.onPrint });
    }
    if (props.hasSelection) {
      list.unshift(
        { id: "archive", label: t("action.archive"), run: props.onArchive },
        { id: "trash", label: t("action.trash"), run: props.onTrash },
        { id: "read", label: t("action.markRead"), run: props.onMarkRead },
        { id: "unread", label: t("action.markUnread"), run: props.onMarkUnread },
        { id: "label", label: t("action.label"), run: props.onLabel },
        { id: "pin", label: t("action.pin"), run: props.onPin },
        { id: "snooze", label: t("action.snooze"), run: props.onSnooze },
        { id: "important", label: t("priority.markImportant"), run: props.onImportant },
        { id: "mute", label: t("action.mute"), run: props.onMute },
      );
    }
    for (const l of Object.values(state.labels)) {
      list.push({
        id: `label:${l.id}`,
        label: `${t("nav.labels")}: ${l.name}`,
        run: () => setView({ kind: "label", labelId: l.id }),
      });
    }
    return list;
  }, [props, setView, state.labels]);

  const matches = useMemo(() => {
    const q = query.trim().toLowerCase();
    if (!q) return commands;
    return commands.filter((c) => c.label.toLowerCase().includes(q));
  }, [commands, query]);

  const run = (cmd: Command | undefined): void => {
    props.onClose();
    if (cmd) cmd.run();
    else if (query.trim()) props.onSearch(query.trim());
  };

  return (
    <div className="palette" role="dialog" aria-label={t("palette.title")} aria-modal="true">
      <input
        ref={inputRef}
        className="palette-input"
        value={query}
        placeholder={t("palette.placeholder")}
        aria-label={t("palette.placeholder")}
        aria-controls="palette-list"
        onChange={(ev) => {
          setQuery(ev.target.value);
          setIndex(0);
        }}
        onKeyDown={(ev) => {
          if (ev.key === "ArrowDown") {
            ev.preventDefault();
            setIndex((i) => Math.min(i + 1, matches.length - 1));
          } else if (ev.key === "ArrowUp") {
            ev.preventDefault();
            setIndex((i) => Math.max(i - 1, 0));
          } else if (ev.key === "Enter") {
            ev.preventDefault();
            run(matches[index]);
          }
        }}
      />
      <ul className="palette-list" id="palette-list" role="listbox">
        {matches.map((c, i) => (
          <li key={c.id}>
            <button
              type="button"
              role="option"
              aria-selected={i === index}
              className={`palette-item ${i === index ? "is-active" : ""}`}
              onMouseEnter={() => setIndex(i)}
              onClick={() => run(c)}
            >
              {c.label}
            </button>
          </li>
        ))}
        {matches.length === 0 ? <li className="palette-empty">{t("palette.noResults")}</li> : null}
      </ul>
    </div>
  );
}
