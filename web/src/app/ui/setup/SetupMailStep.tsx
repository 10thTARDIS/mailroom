// SPDX-License-Identifier: AGPL-3.0-only

// STEP 2 OF 4 — THE MAIL ALREADY IN THOSE FOLDERS (#446).
//
// The only step in day-one that writes, which is why it is the one built most
// carefully around the report:
//
//   the window        7 / 14 / 30 / 90 days, and how much would stay on the desk
//   everything older  labelled and archived IN MAILROOM, with nothing moved
//   per folder        an override each: label and archive / bring to the desk
//                     (which IS a move on the server, said in those words) /
//                     leave alone, to import later
//   the sentence      exactly what this would write, from `Setup/preview`
//   the button        and only then the button
//
// ⚠ THE PREVIEW IS RE-ASKED ON EVERY CHANGE, DEBOUNCED, AND THE BUTTON WAITS FOR
// IT. Applying a plan whose consequences nobody has seen is the thing this step
// exists to prevent (the same rule the desk's own line control keeps —
// ui/settings/rows/LineRow.tsx). A failed preview is still a state to decide
// from, so it says so and leaves the button reachable; a preview in flight is not,
// so the button waits.
//
// ⚠ IT POLLS `Setup/get` WHILE THE RUN DRAINS, every two seconds, rather than
// subscribing to the SetupRun type through the sync engine. The server pushes the
// type (internal/api/eventsource.go lists store.ObjectSetupRun for exactly this
// client), so the stream is the better answer in principle — but taking it would
// mean a new ChangeType, a cursor for it, a store slot, a boot-time read to
// establish that cursor, and a follow-up `Setup/get` in the poll loop: engine
// surgery whose failure mode is a 400 that drops the WHOLE event stream, for a
// surface a login sees once. The engine's own fallback poll is 10 s, which is too
// slow to watch a progress bar with, so this asks directly and stops the moment
// the run is finished. Moving to the stream later changes this file only.

import { useCallback, useEffect, useRef, useState } from "react";
import type { JSX } from "react";

import type { FolderCensus, SetupAction, SetupCounts, SetupRun } from "../../../core/jmap/types";
import { navigate } from "../../routing";
import { useApp } from "../../state";
import { t } from "../../i18n";
import type { MessageKey } from "../../i18n";
import { SetupFooter, StepBody } from "./SetupRoute";
import type { StepProps } from "./SetupRoute";
import { SETUP_WINDOWS, choiceFor, planFor } from "./setupState";
import { countText, wouldSentence } from "./sentences";

/** How long a change waits before the server is asked what it would do. The
 * settings page's own preview debounce is 250 ms and the desk's is the same
 * number; #446 asks for 300, which is the right end of the same range for a
 * control whose answer is a paragraph rather than two counts. */
const PREVIEW_DEBOUNCE_MS = 300;

/** How often a draining run is re-read. See the file header for why this is a
 * poll and not a subscription. */
const RUN_POLL_MS = 2000;

/** The three actions, in the order the design lists them: the safe one, the one
 * with a provider-visible consequence, and the one that defers. */
const ACTIONS: readonly SetupAction[] = ["labelArchive", "bringToDesk", "leave"];

/** Whether a run is still going, which is the same test the server applies when it
 * refuses a second one (`setupRunBusy`) and when it refuses an undo. */
function isLive(run: SetupRun | null): boolean {
  return run !== null && (run.status === "queued" || run.status === "running" || run.status === "undoing");
}

export function SetupMailStep(props: StepProps): JSX.Element {
  const { state, dispatch, census, windowDays, importFolder, onNext, onBack, onSkip } = props;
  const { actions } = useApp();

  const [counts, setCounts] = useState<SetupCounts | null>(null);
  /** `true` while a preview is in flight; the primary button waits on it. */
  const [previewing, setPreviewing] = useState(true);
  const [run, setRun] = useState<SetupRun | null>(null);
  /** The run could not be READ — a different fact from there being no run, and the
   * only one of the two worth a sentence. */
  const [runUnreadable, setRunUnreadable] = useState(false);
  const [busy, setBusy] = useState(false);
  const [refusal, setRefusal] = useState<string | null>(null);

  const restrictTo = importFolder?.mailboxId;
  const plan = planFor(state, census, restrictTo);
  /**
   * The plan as a string, and a ref to the plan itself.
   *
   * `planFor` builds a fresh object on every render, so a `useEffect` dependency
   * on the object would fire a request per render — a preview loop against the
   * server. The STRING is the dependency (it changes exactly when the plan does)
   * and the REF is what the request actually sends, so the debounce is never
   * skipped and the body is never a stale copy.
   */
  const planKey = JSON.stringify(plan);
  const planRef = useRef(plan);
  planRef.current = plan;

  // -- the preview -----------------------------------------------------------
  useEffect(() => {
    let cancelled = false;
    setPreviewing(true);
    const handle = setTimeout(() => {
      void actions.setupPreview(planRef.current).then((res) => {
        if (cancelled) return;
        setCounts(res);
        setPreviewing(false);
      });
    }, PREVIEW_DEBOUNCE_MS);
    return () => {
      cancelled = true;
      clearTimeout(handle);
    };
  }, [actions, planKey]);

  // -- the run, and watching it ---------------------------------------------
  const readRun = useCallback(
    async (id: string) => {
      const runs = await actions.setupRuns([id]);
      if (runs === null) {
        setRunUnreadable(true);
        return;
      }
      setRunUnreadable(false);
      setRun(runs[0] ?? null);
    },
    [actions],
  );

  // A run recorded in the draft is picked back up on arrival, so a reload lands on
  // the progress panel rather than on a button that would start a second run and
  // be refused.
  const runId = state.runId;
  useEffect(() => {
    if (!runId) return;
    void readRun(runId);
  }, [runId, readRun]);

  const live = isLive(run);
  const watching = useRef(live);
  watching.current = live;
  useEffect(() => {
    if (!runId || !live) return;
    const handle = setInterval(() => {
      if (watching.current) void readRun(runId);
    }, RUN_POLL_MS);
    return () => clearInterval(handle);
  }, [runId, live, readRun]);

  const apply = (): void => {
    setBusy(true);
    setRefusal(null);
    void (async () => {
      const outcome = await actions.setupApply(plan);
      setBusy(false);
      if (outcome.kind === "started") {
        dispatch({ type: "run", runId: outcome.runId });
        return;
      }
      if (outcome.kind === "failed") {
        setRefusal(outcome.message);
        return;
      }
      // BUSY IS NOT A FAILURE: a run is already draining, and telling the user the
      // import failed would invite them to start it again. Find the one that is
      // going and show it.
      setRefusal(t("setup.run.busy"));
      const runs = await actions.setupRuns();
      if (runs === null) {
        setRunUnreadable(true);
        return;
      }
      const outstanding = runs.find((r) => isLive(r)) ?? runs[runs.length - 1] ?? null;
      setRun(outstanding);
      if (outstanding) dispatch({ type: "run", runId: outstanding.id });
    })();
  };

  const undo = (): void => {
    if (!run) return;
    setBusy(true);
    void (async () => {
      const ok = await actions.setupUndo(run.id);
      setBusy(false);
      if (ok) await readRun(run.id);
    })();
  };

  const done = run !== null && (run.status === "done" || run.status === "undone");
  const sentence = wouldSentence(counts);

  /** Import mode ends in "Done" back to Settings; the wizard goes on to step 3.
   * Both are the same position in the footer, and neither is reachable until the
   * run says it finished. */
  const onwards = (): void => {
    if (importFolder) {
      navigate({ page: "settings", section: "areas" });
      return;
    }
    onNext();
  };

  return (
    <StepBody>
      {importFolder ? (
        <>
          <p className="setup-lead">{t("setup.import.title", { folder: importFolder.name })}</p>
          <p className="field-hint">{t("setup.import.explain")}</p>
        </>
      ) : null}

      <WindowChoice
        state={state}
        dispatch={dispatch}
        census={census}
        windowDays={windowDays}
        restrictTo={restrictTo}
      />

      <fieldset className="menu-group">
        <legend>{t("setup.mail.older")}</legend>
        <p className="field-hint">{t("setup.mail.older.explain")}</p>
        <p className="menu-note">
          {t("setup.mail.older.count", {
            n: countText(
              census
                .filter((f) => restrictTo === undefined || f.mailboxId === restrictTo)
                .reduce((sum, f) => sum + f.older, 0),
            ),
          })}
        </p>
      </fieldset>

      <PerFolderOverrides
        state={state}
        dispatch={dispatch}
        census={census}
        restrictTo={restrictTo}
      />

      {refusal === null ? null : (
        <p className="banner banner-warn" role="alert">
          {refusal}
        </p>
      )}

      {/* ONE `role="status"` REGION for the sentence and the progress panel. They
          are the same thing at two moments — what the plan would do, and what the
          run is doing — and two live regions would talk over each other on the
          click that turns one into the other. */}
      <div className="setup-status" role="status">
        {run !== null ? (
          <RunPanel run={run} unreadable={runUnreadable} />
        ) : sentence !== null ? (
          <>
            <p className="setup-status-sentence">{sentence}</p>
            {counts !== null && counts.unclassified > 0 ? (
              <p className="setup-status-note">
                {t("setup.would.unclassified", { n: countText(counts.unclassified) })}
              </p>
            ) : null}
            <p className="setup-status-note">{t("setup.would.undo")}</p>
          </>
        ) : previewing ? (
          <p className="setup-status-sentence">{t("setup.would.measuring")}</p>
        ) : (
          // A failed preview is said out loud and the button stays reachable: "the
          // preview failed" is still a state a person can decide from.
          <p className="setup-status-sentence">{t("setup.would.unmeasured")}</p>
        )}
      </div>

      {run !== null && (run.status === "done" || run.status === "failed") ? (
        <div className="setup-dial-row">
          <button
            type="button"
            className="button button-quiet"
            disabled={busy || run.status !== "done"}
            onClick={undo}
          >
            {t("setup.run.undo")}
          </button>
          <span className="setup-dial-note">{t("setup.run.undoExplain")}</span>
        </div>
      ) : null}

      <SetupFooter
        onBack={onBack}
        onSkip={importFolder ? null : onSkip}
        next={
          run === null
            ? {
                label: t("setup.doIt"),
                onClick: apply,
                // Waits for the preview, not for its success: `previewing` is only
                // true while one is in flight.
                disabled: busy || previewing,
              }
            : {
                label: importFolder ? t("setup.import.done") : t("setup.next.3"),
                onClick: onwards,
                disabled: !done,
              }
        }
      />
    </StepBody>
  );
}

/**
 * "Treat as newly arrived": the window, and how much would stay on the desk.
 *
 * The landing count is the census's own `inWindow`, summed — and the census is
 * re-asked at the chosen window by the route, so this number always comes from the
 * server's own split rather than from a client-side comparison against a cutoff it
 * would have had to compute itself.
 */
function WindowChoice({
  state,
  dispatch,
  census,
  windowDays,
  restrictTo,
}: {
  state: StepProps["state"];
  dispatch: StepProps["dispatch"];
  census: FolderCensus[];
  windowDays: number;
  restrictTo: string | undefined;
}): JSX.Element {
  const rows = census.filter((f) => restrictTo === undefined || f.mailboxId === restrictTo);
  const landing = rows.reduce((sum, f) => sum + f.inWindow, 0);
  // The counts on screen belong to the window the SERVER measured them at. While
  // the chosen window is ahead of it — the radio has moved and the new census has
  // not landed — the sentence would be about a cutoff nobody used, so it says
  // nothing instead of saying the old number under the new radio.
  const measured = windowDays === state.windowDays;

  return (
    <fieldset className="menu-group">
      <legend>{t("setup.mail.window")}</legend>
      <div className="setup-dial-row">
        {SETUP_WINDOWS.map((days) => (
          <label key={days} className="radio">
            <input
              type="radio"
              name="setup-window"
              value={days}
              checked={state.windowDays === days}
              onChange={() => dispatch({ type: "window", days })}
            />{" "}
            {t("setup.mail.window.days", { n: days })}
          </label>
        ))}
        <span className="setup-dial-note">
          {measured
            ? t("setup.mail.window.landing", { n: countText(landing) })
            : t("setup.mail.window.landing.unmeasured")}
        </span>
      </div>
    </fieldset>
  );
}

/** One row per folder: a three-way choice and its count. The count is `older` —
 * the mail this step would act on — because a row that showed the whole folder
 * would be offering a decision about a number that includes mail day-one never
 * touches. */
function PerFolderOverrides({
  state,
  dispatch,
  census,
  restrictTo,
}: {
  state: StepProps["state"];
  dispatch: StepProps["dispatch"];
  census: FolderCensus[];
  restrictTo: string | undefined;
}): JSX.Element {
  const rows = census.filter((f) => restrictTo === undefined || f.mailboxId === restrictTo);
  return (
    <fieldset className="menu-group">
      <legend>{t("setup.mail.perFolder")}</legend>
      <p className="field-hint">{t("setup.mail.perFolder.explain")}</p>
      <ul className="setup-rows">
        {rows.map((folder) => (
          <li key={folder.mailboxId} className="setup-row">
            <span className="setup-row-name">{folder.name}</span>
            <span className="setup-row-count">{countText(folder.older)}</span>
            <select
              className="select"
              value={choiceFor(state, folder).action}
              aria-label={t("setup.action.for", { folder: folder.name })}
              onChange={(e) =>
                dispatch({
                  type: "folderAction",
                  mailboxId: folder.mailboxId,
                  action: e.target.value as SetupAction,
                  folder,
                })
              }
            >
              {ACTIONS.map((action) => (
                // bringToDesk is meaningless on INBOX — the inbox IS the desk —
                // and the server refuses it with invalidArguments, so it is not
                // offered rather than offered and always refused.
                folder.role === "inbox" && action === "bringToDesk" ? null : (
                  <option key={action} value={action}>
                    {t(`setup.action.${action}` as MessageKey)}
                  </option>
                )
              ))}
            </select>
          </li>
        ))}
      </ul>
    </fieldset>
  );
}

/**
 * THE PROGRESS PANEL: planned beside actual, per verb.
 *
 * "labels 1,204 / 4,404" rather than a percentage, because the two numbers are the
 * whole point — the preview promised the second one and the run is delivering the
 * first, and that is the only form in which the promise can be CHECKED rather than
 * trusted (internal/api/setup.go says the same from the other side).
 *
 * ⚠ MOVED MAIL IS NOT ON THE DESK YET, and the panel must not say it is. A
 * `bringToDesk` message reaches the desk only after the writeback lands on the
 * provider and the next sync updates its provenance, so the moved clause carries
 * that sentence rather than a promise of an instant change.
 */
function RunPanel({ run, unreadable }: { run: SetupRun; unreadable: boolean }): JSX.Element {
  return (
    <>
      <p className="setup-status-sentence">
        {t(`setup.run.state.${run.status}` as MessageKey)}
      </p>
      <ul className="setup-progress">
        <li>
          {t("setup.run.labels", {
            done: countText(run.labelsWritten),
            planned: countText(run.plannedLabels),
          })}
        </li>
        <li>
          {t("setup.run.archived", {
            done: countText(run.archived),
            planned: countText(run.plannedArchived),
          })}
        </li>
        <li>
          {t("setup.run.moved", {
            done: countText(run.moved),
            planned: countText(run.plannedMoves),
          })}
        </li>
        <li>{t("setup.run.left", { n: countText(run.leftForLater) })}</li>
      </ul>
      {run.moved > 0 ? (
        <p className="setup-status-note">
          {t("setup.run.moveNote", { n: countText(run.moved) })}
        </p>
      ) : null}
      {run.status === "failed" ? (
        <p className="setup-status-note">
          {run.error ? t("setup.run.error", { error: run.error }) : t("setup.run.errorNoMessage")}
        </p>
      ) : null}
      {run.status === "queued" || run.status === "running" || run.status === "undoing" ? (
        <p className="setup-status-note">{t("setup.run.watching")}</p>
      ) : null}
      {unreadable ? <p className="setup-status-note">{t("setup.run.unmeasured")}</p> : null}
    </>
  );
}
