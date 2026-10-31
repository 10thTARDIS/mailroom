// SPDX-License-Identifier: AGPL-3.0-only

// STEP 1 OF 4 — WHAT IS IN YOUR MAILBOX (#446).
//
// The report that has to exist before anything else: every folder the provider
// sorted mail into, the numbers a decision needs, and the one big choice about how
// new mail should arrive. It writes at most one thing, and only on Next.
//
// THE TABLE IS A REAL TABLE. Five columns of per-folder facts with a header row is
// what a table is for, and a grid of divs would take the column headings away from
// assistive tech — the "Msgs" number in the third cell of the fourth row is
// meaningless without the word "Msgs" attached to it.
//
// THE SHAPE WORD IS NEVER THE WHOLE CELL. `shape` is a judgement over the counts
// beside it (internal/store/setup_census.go), so the cell carries the numbers it
// came from and the row carries the raw count as well. A folder the census could
// say nothing about — no From addresses at all — says THAT rather than being
// described as mixed.
//
// ⚠ WITH NO PLAIN FOLDERS THERE IS NO TABLE AND NO DECISION. A login whose
// provider does no folder sorting (a small test instance) has INBOX and
// nothing else: the sentence says so outright, and the two cards are hidden
// because "should folders feed the desk?" is not a question about a mailbox with
// no folders. Rendering an empty table with a decision above it would be asking
// somebody to choose between two answers that are the same answer.

import { useState } from "react";
import type { JSX } from "react";

import type { Area, FolderCensus, Id } from "../../../core/jmap/types";
import { useMailState, useApp } from "../../state";
import { t } from "../../i18n";
import { useAccounts } from "../useAccounts";
import { SetupFooter, StepBody } from "./SetupRoute";
import type { StepProps } from "./SetupRoute";
import { choiceFor, foldersToFeed } from "./setupState";
import type { AreaChoice } from "./setupState";
import { censusSentence, countText, shapeSentence } from "./sentences";

/** The `<select>` option values. Prefixed so an area id can never collide with
 * one of the two special choices. */
const AREA_NONE = "none";
const AREA_NEW = "new";
const AREA_PREFIX = "area:";

function areaValue(choice: AreaChoice): string {
  switch (choice.kind) {
    case "none":
      return AREA_NONE;
    case "new":
      return AREA_NEW;
    case "existing":
      return `${AREA_PREFIX}${choice.areaId}`;
  }
}

function areaFromValue(value: string, folder: FolderCensus): AreaChoice {
  if (value === AREA_NONE) return { kind: "none" };
  if (value === AREA_NEW) return { kind: "new", name: folder.name };
  return { kind: "existing", areaId: value.slice(AREA_PREFIX.length) };
}

/** INBOX FIRST, then the rest in the order the server gave them (display name,
 * then id). The inbox leads because it is the pile step 2 is mostly about, and
 * because it is the row whose Area and Label cells are not controls. */
function inboxFirst(folders: readonly FolderCensus[]): FolderCensus[] {
  return [...folders].sort((a, b) => {
    if (a.role !== b.role) return a.role === "inbox" ? -1 : 1;
    return 0;
  });
}

export function SetupCensusStep(props: StepProps): JSX.Element {
  const { state, dispatch, census, windowDays, onNext, onBack, onSkip } = props;
  const { actions } = useApp();
  const mail = useMailState();
  const { accounts } = useAccounts();
  const [busy, setBusy] = useState(false);

  const areas: Area[] = Object.values(mail.areas).sort((a, b) =>
    a.position !== b.position ? a.position - b.position : a.id.localeCompare(b.id),
  );
  const plain = census.filter((f) => f.role !== "inbox");

  /** One group per MAIL account, in the accounts snapshot's own order, so the
   * sections here appear in the same order as the account switcher's rows. */
  const byAccount: { id: Id; name: string; folders: FolderCensus[] }[] = [];
  for (const f of census) {
    let group = byAccount.find((g) => g.id === f.accountId);
    if (!group) {
      group = {
        id: f.accountId,
        // The account's own name when it has loaded; the id is a last resort and
        // never a guess at which account this is.
        name: accounts.find((a) => a.id === f.accountId)?.name ?? f.accountId,
        folders: [],
      };
      byAccount.push(group);
    }
    group.folders.push(f);
  }
  const several = byAccount.length > 1;

  /**
   * "Folders feed the desk" is the one write step 1 makes, and it is made HERE on
   * Next rather than as each radio is pressed: the decision is about the plan as a
   * whole, and a `Mailbox/set` per keystroke would point folders at the desk that
   * the user then marked "leave alone".
   *
   * "Let Mailroom sort" writes NOTHING AT ALL, deliberately. The user turns the
   * provider's rules off themselves — Mailroom cannot edit them — so a client that
   * also flipped `feedsDesk` off would be claiming to have done the half it
   * cannot do, and the folders would then feed nothing while the provider went on
   * filing mail into them.
   */
  const next = (): void => {
    if (state.feed !== "folders") {
      onNext();
      return;
    }
    const ids = foldersToFeed(state, census);
    if (ids.length === 0) {
      onNext();
      return;
    }
    setBusy(true);
    void (async () => {
      for (const id of ids) await actions.setMailboxFeedsDesk(id, true);
      setBusy(false);
      onNext();
    })();
  };

  return (
    <StepBody>
      <p className="setup-lead">{censusSentence(census)}</p>

      {plain.length === 0 ? (
        <p className="field-hint">{t("setup.census.inboxOnly.explain")}</p>
      ) : (
        <>
          {byAccount.map((group) => (
            <section
              key={group.id}
              className="setup-account"
              {...(several ? { "aria-label": group.name } : {})}
            >
              {several ? <h4 className="setup-account-name">{group.name}</h4> : null}
              <table className="setup-table">
                <caption className="setup-table-caption">{t("setup.table.caption")}</caption>
                <thead>
                  <tr>
                    <th scope="col">{t("setup.table.folder")}</th>
                    <th scope="col">{t("setup.table.msgs")}</th>
                    <th scope="col">{t("setup.table.looksLike")}</th>
                    <th scope="col">{t("setup.table.area")}</th>
                    <th scope="col">{t("setup.table.label")}</th>
                  </tr>
                </thead>
                <tbody>
                  {inboxFirst(group.folders).map((folder) => (
                    <FolderRow
                      key={folder.mailboxId}
                      folder={folder}
                      areas={areas}
                      windowDays={windowDays}
                      state={state}
                      dispatch={dispatch}
                    />
                  ))}
                </tbody>
              </table>
            </section>
          ))}

          <FeedDecisionCards state={state} dispatch={dispatch} />
        </>
      )}

      <SetupFooter
        onBack={onBack}
        onSkip={onSkip}
        next={{ label: t("setup.next.2"), onClick: next, disabled: busy }}
      />
    </StepBody>
  );
}

/** One folder's row. The header cell is the folder NAME (`scope="row"`), so every
 * number in the row is announced against the folder it is about. */
function FolderRow({
  folder,
  areas,
  windowDays,
  state,
  dispatch,
}: {
  folder: FolderCensus;
  areas: Area[];
  windowDays: number;
  state: StepProps["state"];
  dispatch: StepProps["dispatch"];
}): JSX.Element {
  const choice = choiceFor(state, folder);
  const isInbox = folder.role === "inbox";

  return (
    <tr>
      <th scope="row">{folder.name}</th>
      <td className="setup-num">{countText(folder.live)}</td>
      <td>
        {shapeSentence(folder)}
        {/* THE INBOX'S OWN CLAUSE: how much of it arrived inside the window, which
            is the part step 2 leaves alone. It is the inbox's most useful number
            and the reason the row is here at all. */}
        {isInbox ? (
          <>
            {" · "}
            {t("setup.census.inWindow", { n: countText(folder.inWindow), days: windowDays })}
          </>
        ) : null}
        {/* Counted, never hidden: a folder that is 40% unclassifiable is a folder
            whose shape word means much less than it looks like it does. */}
        {folder.unclassified > 0 ? (
          <>
            {" · "}
            {t("setup.census.unclassified", { n: countText(folder.unclassified) })}
          </>
        ) : null}
      </td>
      <td>
        {isInbox ? (
          // The inbox has no folder area: its mail lands in whatever area claims
          // it, per message, by rule or classifier. A select here would be
          // offering a decision the server does not take.
          <span className="setup-fixed">{t("setup.census.byRule")}</span>
        ) : (
          <select
            className="select"
            value={areaValue(choice.area)}
            aria-label={t("setup.area.for", { folder: folder.name })}
            onChange={(e) =>
              dispatch({
                type: "folderArea",
                mailboxId: folder.mailboxId,
                area: areaFromValue(e.target.value, folder),
                folder,
              })
            }
          >
            {areas.map((area) => (
              <option key={area.id} value={`${AREA_PREFIX}${area.id}`}>
                {area.name}
              </option>
            ))}
            <option value={AREA_NEW}>{t("setup.area.new", { name: folder.name })}</option>
            <option value={AREA_NONE}>{t("setup.area.none")}</option>
          </select>
        )}
      </td>
      <td>
        {isInbox ? (
          <span className="setup-fixed">{t("setup.census.noLabel")}</span>
        ) : (
          <input
            type="text"
            className="text-input setup-label-input"
            value={choice.labelName}
            aria-label={t("setup.label.for", { folder: folder.name })}
            onChange={(e) =>
              dispatch({
                type: "folderLabel",
                mailboxId: folder.mailboxId,
                labelName: e.target.value,
                folder,
              })
            }
          />
        )}
      </td>
    </tr>
  );
}

/**
 * THE ONE BIG DECISION, as two cards that are really two radios.
 *
 * REAL RADIOS IN A REAL FIELDSET, with the legend as the question. The cards are
 * clickable and the chosen one is marked, but the marking is a word and a border,
 * never colour alone: `aria-checked` comes free from the input, and the "chosen"
 * class only thickens what is already there.
 */
function FeedDecisionCards({
  state,
  dispatch,
}: {
  state: StepProps["state"];
  dispatch: StepProps["dispatch"];
}): JSX.Element {
  return (
    <fieldset className="menu-group setup-cards">
      <legend>{t("setup.feed.legend")}</legend>
      <label className={`setup-card${state.feed === "folders" ? " is-chosen" : ""}`}>
        <span className="setup-card-head">
          <input
            type="radio"
            className="radio"
            name="setup-feed"
            value="folders"
            checked={state.feed === "folders"}
            onChange={() => dispatch({ type: "feed", feed: "folders" })}
          />
          <span className="setup-card-title">{t("setup.feed.folders")}</span>
          {/* The recommendation is a WORD, not an accent colour. */}
          <span className="setup-card-note">{t("setup.feed.recommended")}</span>
        </span>
        <span className="setup-card-body">{t("setup.feed.folders.explain")}</span>
      </label>
      <label className={`setup-card${state.feed === "mailroom" ? " is-chosen" : ""}`}>
        <span className="setup-card-head">
          <input
            type="radio"
            className="radio"
            name="setup-feed"
            value="mailroom"
            checked={state.feed === "mailroom"}
            onChange={() => dispatch({ type: "feed", feed: "mailroom" })}
          />
          <span className="setup-card-title">{t("setup.feed.mailroom")}</span>
        </span>
        <span className="setup-card-body">{t("setup.feed.mailroom.explain")}</span>
      </label>
    </fieldset>
  );
}
